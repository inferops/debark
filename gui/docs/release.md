# Release

How Debark is built, signed and published — and, more importantly, **exactly
what reproducibility property that gets you**, because the one the core
repository claims does not transfer here unchanged and copying its sentence over
would have been dishonest.

Configuration: [`.goreleaser.yaml`](../.goreleaser.yaml).
Workflow: [`../.github/workflows/release.yml`](../../.github/workflows/release.yml).
Proof: [`hack/reproducible-check.sh`](../hack/reproducible-check.sh).

---

## 1. What a release is

Pushing commits to `main` runs CI but does not create or update a release.
Published downloads belong to a version tag and stay at that version until a
new tag is pushed. Move the changes from `Unreleased` into a dated version
section in the root `CHANGELOG.md` before tagging the release commit.

Push a `vX.Y.Z` tag. **One tag produces one GitHub release carrying both
products.** `release.yml` runs two goreleaser jobs in sequence on pinned
`ubuntu-24.04` runners: the `cli` job runs `../.goreleaser.yaml` and *creates*
the release, then the `gui` job — `needs: cli` — runs this project's
`.goreleaser.yaml` with `release.mode: append` and adds this application's
artefacts to the release the first job made. The rest of this document is about
the second job.

It:

1. checks out this repository **once**. The engine is in the same checkout,
   resolved through `replace github.com/inferops/debark => ../` (§5), so there
   is no second repository to fetch and no pin to resolve;
2. installs the Go toolchain named by `go.mod`, plus `libgtk-3-dev` and
   `libwebkit2gtk-4.1-dev`, and **records the resolved versions of the C
   compiler, the linker and those libraries into the job summary**, because they
   are inputs to the Linux binary and nothing pins them;
3. runs `hack/reproducible-check.sh linux windows`, which builds both targets
   twice and fails the release if the outputs differ;
4. runs `goreleaser release --clean`, which builds the two binaries, calls
   `packaging/build-deb.sh` to produce the `.deb`, writes the archives and
   `debark-gui_checksums.txt`, signs that file with keyless cosign, and attaches
   an SBOM per archive, then appends all of it to the release the `cli` job
   created;
5. hands `debark-gui_checksums.txt` to `slsa-github-generator` for build
   provenance.

Locally, `make snapshot` runs the whole thing without a tag and without
publishing anything; `make release-check` validates the configuration and builds
nothing.

### Artefacts

| Artefact | Target | Status |
|---|---|---|
| `debark-gui_<version>_linux_amd64.tar.gz` | linux/amd64 | **Supported.** |
| `debark-gui_<version>_amd64.deb` | linux/amd64 | **Supported.** The deliverable. |
| `debark-gui_<version>_windows_amd64.zip` | windows/amd64 | Built, **not supported**. |
| `debark-gui_checksums.txt`, `.sig`, `.pem` | — | sha256 over everything above, cosign-signed. The CLI half of the same release publishes its own `debark_checksums.txt`. |
| `*.spdx.sbom.json` | per archive | syft. |
| `debark-gui.intoto.jsonl` | — | SLSA build provenance. |
| `PROVENANCE.txt` | inside every archive | The commit both halves were built from. See §5. |

**linux/arm64 is deliberately absent.** Building it needs an arm64 cross
toolchain *and* an arm64 GTK/WebKitGTK sysroot, and shipping an arm64 binary
nobody has ever started would be worse than not shipping one. This is an open
item (§8), not a decision that arm64 does not matter.

macOS is out of scope for this application, as it is in `ci.yml`'s matrix.

---

## 2. One producer for the `.deb`, and it is not goreleaser

goreleaser has an `nfpms:` pipe that builds `.deb` and `.rpm` packages, and
`../.goreleaser.yaml` uses it. **This repository deliberately does
not.** `.goreleaser.yaml` has no `nfpms:` section; the release calls
`packaging/build-deb.sh` from the Linux build's post hook instead.

The reason is not taste. `docs/security-review.md` §5 is a twenty-row checklist
about what the package must not contain — no setuid file, no maintainer script,
no polkit action, no sudoers fragment, nothing outside five directories — and
`packaging/verify-deb.sh` runs it against a built `.deb`. If nfpm produced the
released package and `build-deb.sh` produced the one anyone verified, then **the
thing that was proven and the thing that shipped would be two different
artefacts**. `build-deb.sh` is standalone (`dpkg-deb` alone, no goreleaser) so
that the package stays buildable and provable by anyone with a Debian box, and
that only means anything if it is also the package that ships.

The interface between the two, agreed with the packaging work and implemented
as environment variables rather than positional arguments so neither side can misread the
other's argument order:

| Variable | Value goreleaser passes |
|---|---|
| `DEBARK_GUI_BINARY` | absolute path to the built linux/amd64 binary |
| `DEBARK_GUI_VERSION` | the release version, no leading `v` |
| `DEBARK_GUI_ARCH` | goreleaser's arch name (`amd64`) |
| `DEBARK_GUI_OUTDIR` | `dist` — where to write the `.deb` |
| `SOURCE_DATE_EPOCH` | the tag commit's timestamp |

`build-deb.sh` must write exactly one `*.deb` into `$DEBARK_GUI_OUTDIR` and
exit non-zero on any failure. `.goreleaser.yaml`'s `checksum.extra_files` and
`release.extra_files` then pick it up by glob, which is what puts the package's
digest inside `debark-gui_checksums.txt` and therefore **under the cosign
signature**: verifying the signature on `debark-gui_checksums.txt` and then the
`.deb` against that file authenticates the package transitively, exactly as it
does the archives.

If `packaging/build-deb.sh` were missing, `make snapshot` would fail at that
hook rather than quietly produce no package. That is intentional and is the
same choice the core repository makes with its completions/man hook: silently
shipping no `.deb` is worse than a red build.

**The hook has now been run for real**, not with `--skip=post-hooks`: a full
`goreleaser release --snapshot` calls the script, the script writes the one
`.deb` into `dist/`, `checksum.extra_files` picks it up, and the binary inside
the package is byte-for-byte the binary inside the archive. §4.8 and §4.9 are
the measurements.

---

## 3. `wails build` is not what produces the release

goreleaser builds with the Go toolchain directly, passing
`-tags desktop,production,webkit2_41`. `wails build` is not in the release path.

**The tags are the whole point.** Without `desktop,production` the binary
compiles and then refuses to run, printing *"Wails applications will not build
without the correct build tags"*. A release that shipped that would be worse
than no release, so it was checked rather than assumed — see §4.6.

What `wails build` does that this does not:

- On **Linux**: nothing that matters. It runs the same `go build` with the same
  tags after the frontend copy step, which `.goreleaser.yaml`'s `before` hook
  does itself (`go run ./hack/copyfrontend` — contract rule 5, there is no
  bundler and never an `npm run build`).
- On **Windows**: it also compiles `build/windows/icon.ico` and
  `build/windows/info.json` into a `.syso` with winres, giving the executable its
  icon and its DPI-awareness manifest. goreleaser cannot run winres, and adding
  `go-winres` would be a new build-time Go dependency, which contract rule 6 says
  to report rather than adopt. **So the released Windows binary has no icon and
  no DPI manifest.** That is now a decision with a reason — §3.1 — rather than a
  gap in a report.

CI still runs a real `wails build` on every push, so "does `wails build` still
work" stays answered by the thing that is supposed to answer it.

### 3.1 The Windows icon and DPI manifest: accepted, not deferred

This was carried as an open item for a while. It is closed here as a decision,
because leaving it open is how a known gap turns into a forgotten one.

**The release ships a Windows binary with no icon and no DPI-awareness
manifest, deliberately.** Concretely that means: Explorer and the taskbar show
the generic executable icon, and on a display scaled above 100% Windows applies
DPI virtualisation — the window is rendered at 96 dpi and bitmap-stretched, so
text is soft rather than mis-laid-out.

The reasons, in the order they carried weight:

1. **Windows is built, not supported.** `ci.yml`, `.goreleaser.yaml` and the
   release notes all say so in the same words, and this repository has never
   made a support commitment for it. Spending a new dependency on the polish of
   an unsupported artefact inverts the priority: the `.deb` is the deliverable.
2. **The fix is a new build-time Go dependency.** `go-winres` would be the
   first tool this project adopted for cosmetics. Contract rule 6 says report
   it rather than adopt it, and `docs/dependency-review.md` — which asks "what
   would its compromise mean?" of every tool — has to answer that question for
   a code generator that emits an object file linked into the shipped binary.
   It is reported there, with the answer, and declined.
3. **The alternative — committing a generated `.syso`** — is worse, not better.
   A checked-in object file that nothing in the tree can regenerate is an
   opaque blob in the link, and `build/` is not this file's to write anyway.
4. **Nothing here can prove the fix works.** The harness is Linux/WebKit2GTK;
   CI's Windows job builds and tests but never starts the application. A
   winres-produced manifest is exactly the kind of change whose only real test
   is looking at the window on a scaled display, and this package will not ship a
   change it cannot run. That is the same standard that decided §4.7.

**What would reverse it**, so the decision is falsifiable rather than
permanent: a support commitment for Windows, or a Windows installer (there is
none — `docs/packaging.md` §8), or someone who can actually run the result on a
scaled display doing the work. `wails build` already produces both, so a
developer building locally is unaffected; this is a property of the *released*
archive only. Recorded in the release notes footer so a person who downloads
the zip is told rather than surprised.

---

## 4. What reproducibility means here — measured

The core repository's claim is: `CGO_ENABLED=0`, `-trimpath`, `mod_timestamp`
from the commit, therefore same source + same Go toolchain ⇒ byte-identical
binary, anywhere.

**That claim does not transfer to the Linux build of this application, and
saying it would be false.** Debark renders through WebKitGTK and the Wails
binding is cgo, so the Linux binary is additionally a function of the C
compiler, the system linker and the system headers — none of which this
repository pins, and all of which apt resolves at build time. The honest thing
to do was to find out what is actually true, so it was run and diffed rather
than asserted.

### 4.1 The measurement setup

Two machines:

- **Host:** Windows 11 Pro 10.0.26200, Go 1.26.0 windows/amd64.
- **Container:** Ubuntu 24.04.4, Go 1.26.0 linux/amd64, gcc 13.3.0
  (`Ubuntu 13.3.0-6ubuntu2~24.04.1`), GNU ld 2.42,
  `libc6-dev 2.39-0ubuntu8.8`, `libgtk-3-dev 3.24.41-4ubuntu1.3`,
  `libwebkit2gtk-4.1-dev 2.52.6-0ubuntu0.24.04.1`. The rows below that predate
  `-buildmode=pie` were taken in `debark-shots:go126`; everything re-measured
  for pie, and everything involving a package, was taken in
  `debark-deb-build:noble`, which is the same Ubuntu, Go, gcc, ld, glibc, GTK
  and WebKitGTK and additionally carries `dpkg-dev` and `lintian`.

**A methodological note that turned out to matter.** The first attempt to
measure path-invariance appeared to find the Linux and Windows digests both
changing between two container runs. They did — and the cause was not the path.
The working trees are live: between the two runs another package had committed to
the sibling `debark` repository (`6549f6fe` → `bf519149`) and had files
mid-edit in both trees, one of them not even parseable. **The source changed, so
of course the binary did.** Every controlled measurement below was therefore
re-run against a frozen snapshot — `git archive HEAD` tarballs of both
repositories, expanded inside the container, so that exactly one variable moves
at a time:

- the pre-pie rows: `debark-gui` `9770e74e1c77d03b1b50d3fbb9562f11a10aa04f`,
  `debark` `bf519149f1af8a8efc0aae8e54f89cce2cea32bf`
- everything re-measured for `-buildmode=pie` (§4.7 and the digest block
  below): `debark-gui` `29fd696aa33db13fe73e134ef6fa5237a3b6a830`,
  `debark` `a529c3264572ab3e068dfebbb0a6400cb212ee08`

Those tarballs carry no `.git`, so those builds used `-buildvcs=false`. That is
noted rather than hidden: the VCS stamp is a separate, well-understood input
(same commit and same dirty flag ⇒ same stamp), and it is not what these
experiments are about.

**A correction to the method, found while re-measuring.** At the time these
digests were taken there was no `.gitattributes`, and `git archive HEAD` run on
this Windows host with `core.autocrlf=true` — the developer default here —
**rewrote every text file in the tarball to CRLF**, including the Go sources.
Builds from such a tarball are internally consistent, so every *verdict* taken
that way stands, but their digests are not the digests anyone gets from a clean
LF checkout, and one of them was a shell script the container then refused to
execute (`/usr/bin/env: 'bash\r': No such file or directory`), which is how it
was noticed. Every digest quoted below was therefore taken from
`git -c core.autocrlf=false -c core.eol=lf archive`, so it is reproducible from
the repository rather than from this machine's git configuration. The digests
printed in an earlier revision of this document were not, and they have been
replaced rather than annotated.

**That hole has since been closed by someone else, for a different reason.**
The design work landed a `.gitattributes` with `eol=lf` on `*.go`, `*.sh`,
`*.md`, `Makefile`, `frontend/**` and `packaging/**`, because WebKit's CSS
tokenizer reads a CRLF inside a multi-line custom-property value as two
newlines. Checked here rather than assumed: a plain `git archive HEAD` on this
host now yields zero CR bytes in `main.go`, `hack/reproducible-check.sh`,
`packaging/build-deb.sh` and this file. So the `-c core.autocrlf=false` above
is now belt and braces rather than load-bearing. It is left in the recipe,
because a reproducibility experiment should not depend on a file it does not
control. **`*.yaml` and `*.yml` are not in that list** — harmless for
`.goreleaser.yaml`, less obviously harmless for the `run:` blocks in
`.github/workflows/`, and reported rather than changed since `.gitattributes`
is not this package file.

This is also, incidentally, the single best argument for §5: an unhashed
dependency read off local disk changed under a measurement and the measurement
had no way to notice. `hack/reproducible-check.sh` now prints the sibling's
commit and refuses to let its digests be quoted as release digests when that
checkout is dirty, for exactly this reason.

### 4.2 What was proved

Every row re-taken at `debark-gui` `29fd696a` / `debark` `a529c326`, with
`-buildmode=pie` on linux — which is what §4.7 is about, and the reason the
whole table was re-run rather than annotated.

| # | Property | Result |
|---|---|---|
| 1 | Two builds, same tree, same environment — **linux/amd64, cgo, pie** | **identical** |
| 2 | Two builds, same tree, same environment — **windows/amd64** | **identical** |
| 3 | Same source at two very different absolute paths — linux, pie | **identical** |
| 4 | Same source at two very different absolute paths — windows | **identical** |
| 5 | Cold (wiped) `GOCACHE` — linux, pie | **identical** |
| 6 | Cold (wiped) `GOCACHE` — windows | **identical** |
| 7 | **Different build OS and machine** (Ubuntu 24.04 container vs Windows 11 host) — windows | **identical** |
| 8 | **Different C compiler** (gcc 13.3.0 vs gcc 12.4.0) — linux, pie | **DIFFERENT** |
| 9 | `hack/copyfrontend` run twice — the embedded frontend bundle | **identical** |
| 10 | **`-buildmode=pie` vs not** — linux | **DIFFERENT**, as it must be (§4.7) |
| 11 | Two runs of `packaging/build-deb.sh`, same binary, same `SOURCE_DATE_EPOCH` | **identical** (§4.9) |
| 12 | Two runs of the whole `goreleaser` pipeline — `.deb` and both archives | **identical** (§4.8) |
| 13 | Two runs of the whole pipeline — the two syft SBOMs | **DIFFERENT** (§4.8) |

Digests, from the frozen snapshot, all with `-trimpath -buildvcs=false` and
`.goreleaser.yaml`'s exact `-ldflags` string:

```
linux/amd64, pie,    gcc 13.3.0, /p1                a98af6771a54e869b43b8519ec358e4eabd5829eb07ee3015689f77260ea8821   15524896 bytes
linux/amd64, pie,    gcc 13.3.0, /a/considerably/…  a98af6771a54e869b43b8519ec358e4eabd5829eb07ee3015689f77260ea8821   15524896 bytes
linux/amd64, pie,    gcc 13.3.0, /p1, GOCACHE wiped a98af6771a54e869b43b8519ec358e4eabd5829eb07ee3015689f77260ea8821   15524896 bytes
linux/amd64, pie,    gcc 12.4.0, /p1                b9b58caef60734bd3be315922b590354e4808447682f003c49a1222009761c25   15524896 bytes   <-- differs
linux/amd64, NO pie, gcc 13.3.0, /p1                91458c851f9d98370c7c5aca45c01116af349dd9c6ab392da64dc121e3d52ec5   14113608 bytes   <-- what pie replaced

windows/amd64, /p1                                  d96e9b52c2f0af88f4ccc2aeba59d48eed2874df446c067e703183e9b029e171   15387648 bytes
windows/amd64, /a/considerably/longer/…             d96e9b52c2f0af88f4ccc2aeba59d48eed2874df446c067e703183e9b029e171   15387648 bytes
windows/amd64, /p1, GOCACHE wiped                   d96e9b52c2f0af88f4ccc2aeba59d48eed2874df446c067e703183e9b029e171   15387648 bytes
windows/amd64, built on the Windows 11 host         d96e9b52c2f0af88f4ccc2aeba59d48eed2874df446c067e703183e9b029e171   15387648 bytes

frontend/dist, tree digest, container and host       8e829bcf4068f2d1610b6587d484647c6abb58a77d34ad791978a8121826cf52   23 files
```

Row 8 is still the one worth staring at, and pie did not soften it. **Same
source, same Go toolchain, same flags, same path, same machine, same size — and
a different binary, because the C compiler was gcc 12 instead of gcc 13.** That
is the measurement that decides what this document is allowed to claim, and it
is unchanged by the hardening flag.

Commands, all rerunnable:

```sh
# Rows 1, 2, 9 - the script CI runs. Linux must be run on Linux; the cgo build
# cannot be cross-compiled.
bash hack/reproducible-check.sh windows          # on the Windows host
bash hack/reproducible-check.sh linux windows    # inside the build container

# Rows 3-8 and 10, from git-archive tarballs of the repository, one variable
# at a time. Same flags as .goreleaser.yaml, INCLUDING the order of -ldflags:
GOOS=linux   CGO_ENABLED=1 go build -trimpath -buildvcs=false -buildmode=pie \
  -tags desktop,production,webkit2_41 \
  -ldflags "-s -w -X main.version=0.0.0-reproducible-check" -o OUT .
GOOS=windows CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -tags desktop,production \
  -ldflags "-s -w -H windowsgui -X main.version=0.0.0-reproducible-check" -o OUT.exe .
# Row 8 additionally sets CC=gcc-12 / CC=gcc-13.
# Row 10 drops -buildmode=pie.
```

**"Including the order" is not pedantry, it is a measured trap.** The same
`-ldflags` in a different order produces a different binary:

```
-s -w -H windowsgui -X main.version=…   d96e9b52c2f0af88f4ccc2aeba59d48eed2874df446c067e703183e9b029e171
-s -w -X main.version=… -H windowsgui   3f3e4596e0ba9d2cd4c0b6b4ef76664aa52e0aaaed0352c134ef9f27e92500f0
```

Same size, and `go tool buildid` shows the only difference is the first
component of the build ID — the action-graph hash, which includes the link
flags as a *string*. That is exactly the kind of thing that makes a
reproducibility check disagree with the release for no visible reason, so
`hack/reproducible-check.sh` now assembles its `-ldflags` in `.goreleaser.yaml`'s
order, per target, and carries `-H windowsgui` on windows because the release
does.

### 4.3 The property, stated so it is not overclaimed

**windows/amd64 — the strong claim, and it is the core repository's claim
verbatim.** `CGO_ENABLED=0` (Wails drives WebView2 through the pure-Go
`github.com/wailsapp/go-webview2`), `-trimpath`, `mod_timestamp` from the
commit. *Same source and same Go toolchain version ⇒ byte-identical, on any
machine, any host OS, any path, warm or cold cache.* Proved across two
operating systems (rows 4, 6, 7).

**linux/amd64 — the conditioned claim.** *Same source, same Go toolchain **and
the same C toolchain and system libraries** ⇒ byte-identical, at any path, warm
or cold cache.* In practice "the same C toolchain and system libraries" means
**the same build image**, so:

> The Linux binary is reproducible by anyone who rebuilds it on Ubuntu 24.04
> with the gcc, binutils, glibc, GTK3 and WebKitGTK versions recorded in the
> release's job summary, from the two commits named in `PROVENANCE.txt`.

That is a real, checkable property. It is not "byte-identical for anyone,
anywhere", and this document will not say that it is.

**Both statements were re-proved with `-buildmode=pie` on**, which is the whole
reason §4.2 was re-run rather than annotated: the hardening flag changes the
link, so it was the obvious candidate for silently costing the property. It
did not, and it did not widen or narrow the conditioning either — §4.7.

**The frontend bundle is reproducible unconditionally** (row 9). It is embedded
wholesale with `//go:embed all:frontend/dist`, so if the copy step were not a
deterministic function of the source, nothing downstream could be either;
`hack/reproducible-check.sh` checks it first so that a copy-step defect is
reported as a copy-step defect rather than as a mysterious binary diff.

### 4.4 What follows for the release

- `release.yml` pins `runs-on: ubuntu-24.04`, **not `ubuntu-latest`**. A
  floating runner label is the same class of defect as a floating action tag,
  and here it is worse, because it silently changes an input to the binary.
- The resolved gcc, ld, glibc, GTK3 and WebKitGTK versions are written to the
  job summary on every release. That does not make the build reproducible; it
  makes it **attributable**, which is the achievable half.
- `hack/reproducible-check.sh` runs in the release job and fails it if the two
  builds differ. The claim is checked on every release rather than on the day
  someone wrote it down.

### 4.5 What is *not* proved, and is not claimed

- **Cross-distribution Linux reproducibility.** Only Ubuntu 24.04 was tested.
  A Debian 13 build almost certainly differs; row 8 says a gcc minor version is
  enough.
- **The signature files.** Not checked for determinism, and not expected to be
  byte-stable — Sigstore embeds a timestamp and a fresh ephemeral key. That is
  fine: the signature is verified, not diffed. Signing itself could not be
  exercised locally at all, because keyless cosign wants an OIDC token no
  machine here can mint; every run below used `--skip=sign`. **What the
  signature covers was exercised**: `debark-gui_checksums.txt` is produced, and
  the `.deb` is in it.
- **The SBOMs are now measured, and they are NOT deterministic** — §4.8. That
  is a syft property, not a pipeline defect, but it is the reason
  `debark-gui_checksums.txt` is not byte-stable either.
- **Anything about linux/arm64.** Not built, so not measured.
- **The Windows binary was never *started*.** Every windows claim here is about
  bytes. Nothing in this project can run a Windows GUI binary — see §3.1, which
  is the same fact deciding a different question.

### 4.6 The binary goreleaser produces actually runs

Checked, because the failure mode is silent. `goreleaser build --snapshot
--clean --id linux --single-target --skip=post-hooks` in the build container
produced a binary whose `readelf -d` shows:

```
NEEDED  libwebkit2gtk-4.1.so.0
NEEDED  libgtk-3.so.0
NEEDED  libjavascriptcoregtk-4.1.so.0
NEEDED  libc.so.6
```

— so cgo really is on and the real engine is linked — and running it with no
`DISPLAY` panics with `failed to init GTK`, which is the correct failure for a
GUI with no display. It does **not** print *"Wails applications will not build
without the correct build tags"*, which is the failure this whole section exists
to prevent.

That first check used `--skip=post-hooks`, because `packaging/build-deb.sh` did
not exist yet. **It does now, and the hook has since been exercised for real** —
§4.8 — with the same result: the binary inside the shipped archive and the
binary inside the shipped `.deb` are the same bytes, and that binary starts far
enough to fail on the display it does not have.

The binary was also **started under the real engine**, not merely executed: on
Xvfb with `WEBKIT_DISABLE_COMPOSITING_MODE=1`, the pie build maps a window
titled `Debark` and paints the readiness screen (image standard deviation
0.096, against 0.000 for a flat fill and ~0.09 for a settled screen in
`docs/performance.md`'s own external check). Its `readelf -d` carries
`libwebkit2gtk-4.1.so.0`, `libgtk-3.so.0`, `libjavascriptcoregtk-4.1.so.0`,
`libc.so.6`, and now also `FLAGS BIND_NOW` / `FLAGS_1 NOW PIE`.

`goreleaser check` validates the configuration; `goreleaser build --snapshot
--clean --id windows --single-target` on the Windows host produced the
executable and `dist/PROVENANCE.txt` as intended.

### 4.7 Hardening: `-buildmode=pie` and `-s -w`, measured on both sides

`packaging/verify-deb.sh` — the twenty-row checklist's executable form — ran
`lintian` against the package and reported **`W: hardening-no-pie`**, and a
plain `lintian` run by another package against a hand-built package reported
**`E: unstripped-binary-or-object`** as well. Both are ordinary properties of a
Go binary: Go with cgo defaults to `-buildmode=exe`, and Go does not strip.
Both are also decisions about the same link step, so they were taken together
rather than one at a time.

**They are attributable to flags, not argued about.** Four binaries from one
source, four packages, one `lintian` each:

| `-ldflags` | build mode | `lintian` E and W |
|---|---|---|
| none | exe | `E: unstripped-binary-or-object`, `W: hardening-no-pie` |
| `-s -w` | exe | `W: hardening-no-pie` |
| none | **pie** | `E: unstripped-binary-or-object` |
| **`-s -w`** | **pie** | **none** |

The last row is what `.goreleaser.yaml` builds, and it is clean: no `E`, no
`W`. The informational lines that remain are `binary-has-unneeded-section
.comment` and three `spelling-error-in-binary` hits inside vendored strings.
`no-manual-page` is gone too, because `packaging/build-deb.sh` installs one as
of `08323d5`.

**So the answer on stripping is: this project ships a stripped binary, and
always did.** `-s -w` has been in `.goreleaser.yaml`'s `ldflags` from the
start, matching `..`'s. The `E:` that was found came from a package
built by hand with the command in `build-deb.sh`'s header, which does not carry
the release's ldflags — reproduced above as row 1. **It is a property of that
build, not of the released artefact.** Nothing needed changing, and the reason
it did not is written here so the next person to see the tag does not go
looking for it in the wrong place.

The two costs usually cited for stripping a Go binary were checked rather than
assumed, because "we kept the symbols deliberately" is only worth saying if
losing them would cost something:

- **`go version -m` still works.** On the stripped pie binary it prints the Go
  version, the main module path, and the full `dep`/`=>` graph including
  `github.com/inferops/debark v0.0.0 => .. (devel)`. Build info
  lives in its own section, not in the symbol table.
- **Panic tracebacks are unchanged.** The `failed to init GTK` panic from the
  stripped binary and from the unstripped one are the same text, function for
  function, file for file, line for line —
  `wails/v2@v2.12.0/internal/frontend/desktop/linux/frontend.go:160 +0x138` in
  both. Go's tracebacks come from the pclntab, which `-s -w` does not remove.
  The only difference is the register values, which move because of ASLR.

What `-s -w` does cost is DWARF, so `gdb` on a released binary is degraded.
That is the trade this project already made.

**It costs one more thing, found by running the acceptance test rather than
`lintian` alone, and it is the reason this decision needed writing down at
all.** `packaging/verify-deb.sh` was run end to end against the package the
pipeline produces — rows 19 and 20 included, in a container, as an
unprivileged user with no `sudo`:

| Build | plain `lintian` | `docs/security-review.md` §5 |
|---|---|---|
| pie + `-s -w` — **what ships** | **clean: no `E`, no `W`** | **19 / 20** — row 16 fails |
| pie, unstripped | `E: unstripped-binary-or-object` | **20 / 20** |

**Row 16 is "the binary is not linked against libcap or libpolkit and makes no
setuid call", and it fails on a stripped binary for want of evidence, not
because it found anything.** Every cgo Go binary on Linux imports the same nine
credential-changing symbols from glibc, because `runtime/cgo/linux_syscall.c`
compiles nine `_cgo_libc_set*` wrappers; the row's discriminator is that each
import is matched by one of those wrappers in the binary's own symbol table,
which is exactly what `-s` removes. The script says so itself — it fails with
*"binary is stripped, so the origin of setuid cannot be corroborated here"*,
which is a different sentence from the genuine finding it is also written to
produce, *"setuid is imported with no runtime/cgo wrapper behind it"*.

The corroboration exists; it is one step earlier in the same build. From the
identical source with the identical flags but for `-s -w`:

```
nm <pie, unstripped>  | grep -c _cgo_libc_set   ->  9
nm <pie, stripped>    | grep -c _cgo_libc_set   ->  0
```

and the unstripped package scores **20 / 20**, row 16 included. The two
binaries import the same nine dynamic symbols, neither links `libcap`,
`libcap-ng` or `libpolkit`, and neither carries a capability symbol of any
kind. So the property row 16 asks about holds for the shipped package; what
does not hold is the shipped package's ability to demonstrate it to that
particular command.

**The decision is to keep `-s -w`, and it is deliberate:**

1. `lintian` on the shipped artefact goes fully clean, `E` and `W` both. The
   alternative trades one red for another — an `E` on every release forever,
   against one checklist row that cannot see its evidence.
2. The reason previously given for shipping unstripped — keeping `go version
   -m` provenance — **does not require an unstripped binary**, measured above.
   That was the load-bearing argument and it does not bear.
3. §5.2 is explicit that rows 1–18 are each individually defeatable and that
   **rows 19 and 20 are the two that prove anything**. Both pass: install and
   remove change no permission, group or service state, and the app starts as
   an unprivileged user with `sudo` removed and paints its window.
4. Row 16's corroboration is recoverable at build time, on the pre-strip
   binary, by the two `nm` commands above — and its other half (`go list
   -deps`) is already a build-time check recorded in `docs/packaging.md`
   rather than something the `.deb` can carry.

**What is owed, and to whom.** Row 16 needs a way to corroborate on a stripped
binary — checking that the dynamic import set is *exactly* those nine and
nothing else would do it, and the script already collects that list.
`packaging/verify-deb.sh` is not this package file, so this is reported rather
than changed, with the measurement above as the evidence. Until it is settled,
**the release package is 19/20 with one row unprovable as written**, and that
is stated here rather than rounded up.

One practical note for anyone running that script: **row 20 can hang
indefinitely** in a container with no session bus, because GTK falls back to
`dbus-launch --autolaunch`, which blocks — observed at over ten minutes with no
timeout in the script. Exporting a `DBUS_SESSION_BUS_ADDRESS` from a
`dbus-daemon --session` before running it makes row 20 complete in seconds.

**And pie's cost, measured, because a hardening flag that quietly took the
reproducibility property away would be a bad trade made invisibly:**

| | no pie | pie | delta |
|---|---:|---:|---|
| linux binary | 14,113,608 B | 15,524,896 B | **+1,411,288 B, +10.0%** |
| `.deb` | 4,365,868 B | 4,512,160 B | +146,292 B, +3.4% |
| dynamic relocations | 27,458 | 27,678 | +220, +0.8% |
| `GNU_RELRO` segment | 680 B | 1,406,224 B | the whole GOT, now read-only |
| `DT_FLAGS` | *(absent)* | `BIND_NOW` | `hardening-no-bindnow` also cleared |
| ELF type | `EXEC` | `DYN` | the point of the exercise |
| **two builds, two paths, cold `GOCACHE`** | identical | **identical** | **the property survives** |
| **gcc 13.3.0 vs 12.4.0** | different | **different** | the conditioning is unchanged |

**Cold start: no measurable cost.** Fifteen interleaved A/B pairs on Xvfb,
`spawn → the Debark window is mapped`, which is the leg of cold start that
`exec`, the dynamic linker and GTK window creation live in — 121 ms p50 for
no-pie and 108 ms p50 for pie, mean difference **−5.2 ms with a 95% interval of
[−27, +16] ms**. The interval straddles zero and the point estimate has the
wrong sign for a "pie is slower" story, so the honest statement is: *not
distinguishable from zero at this sample size, against a leg that is itself
about half of the 213 ms cold start in `docs/performance.md` and an eighth of
the 1.5 s budget.* `LD_DEBUG=statistics` agrees from the other direction: the
loader's own reported startup time overlapped completely between the two (5.97
– 6.54 Mcycles for pie, 6.04 – 8.12 for no-pie, three runs each), which is what
+220 relocations on top of 27,458 should look like.

**Windows: measured, deliberately not adopted.** `GOOS=windows CGO_ENABLED=0
-buildmode=pie` builds, and reproduces exactly as well as the non-pie build —
`8fdb4e10f5c5efd50943c5fa0cedf8fc7879ac58a3633048b59eb7a56e9d91e7`, identical
across two builds, two absolute paths, and *both operating systems*, at
15,387,648 bytes, which is byte-for-byte the same size as without it. It would
turn on ASLR for a target that has none, and cost nothing measurable. It is
still not enabled, for the reason in §3.1: nothing here can start a Windows GUI
binary, Windows is built rather than supported, and no `lintian` row is waiting
on it. The digest is recorded so whoever can run it does not have to measure it
again.

### 4.8 The pipeline end to end, twice — and the one thing that still moves

The post hook was exercised for real: `goreleaser release --snapshot --clean
--skip=publish,sign`, with `packaging/build-deb.sh` actually running, twice,
from one frozen snapshot in one container.

| Artefact | Two runs |
|---|---|
| `debark-gui_<v>_amd64.deb` | **identical** |
| `debark-gui_<v>_linux_amd64.tar.gz` | **identical** |
| `debark-gui_<v>_windows_amd64.zip` | **identical** |
| `PROVENANCE.txt` | **identical** |
| `*.spdx.sbom.json` (two of them) | **different** |
| `debark-gui_checksums.txt` | **different**, and only because of the SBOMs |

The SBOM diff is exactly two fields, and nothing else in either file:

```
-  "documentNamespace": "https://anchore.com/syft/file/…-b076da61-f7b8-4e65-a06c-7c4f8e587796"
+  "documentNamespace": "https://anchore.com/syft/file/…-209953b4-90d8-45d6-ae32-6aa1386015be"
-  "created": "2026-09-07T15:34:34Z"
+  "created": "2026-09-07T15:34:47Z"
```

— a fresh UUID and a wall-clock stamp, both from syft, neither describing the
software. So: **every artefact this pipeline ships is byte-reproducible except
the two SBOMs, and `debark-gui_checksums.txt` inherits their instability.** That
is stated rather than fixed. Suppressing it would mean post-processing syft's output,
which would make the SBOM no longer the thing syft produced.

**Getting the archives to that point took a change, and it is worth recording
why.** Before it, two runs of the pipeline produced two different `.tar.gz`
files. The tar payloads differed in exactly two ustar `mtime` fields — the
binary's and `PROVENANCE.txt`'s — because goreleaser reads each member's mtime
off the filesystem, and both of those files are written during the run.
`mod_timestamp` does not cover it: it pins what is *embedded* in the binary,
not the file's mtime. On a CI runner it would be worse than it looks locally,
because `LICENSE`, `README.md` and `docs/release.md` take their mtime from when
`actions/checkout` wrote them. `.goreleaser.yaml` now sets `builds_info.mtime`
and a per-file `info.mtime`, both `{{ .CommitDate }}`, and the archives became
identical.

One trap found doing it, recorded because it silently produces a wrong answer:
**in a git repository with no remote configured, goreleaser's snapshot mode
zeroes its entire git context**, so `{{ .CommitDate }}` renders as
`0001-01-01T00:00:00Z` and `{{ .CommitTimestamp }}` as `-62135596800`, and both
`mod_timestamp` and `info.mtime` are then silently skipped rather than failing.
A real checkout and a CI runner both have a remote, so this only bites a
scratch clone — which is exactly what a reproducibility experiment uses.

### 4.9 The `.deb`, measured — the claim this document used to decline

The previous revision of this section said the package's reproducibility was
"not claimed, because it has not been run twice and diffed". It has now been
run twice and diffed, three separate ways, and the claim is:

> **Two runs of `packaging/build-deb.sh` over the same binary with the same
> `SOURCE_DATE_EPOCH` produce a byte-identical `.deb`** — and so do two runs of
> the whole release pipeline over the same source.

Digest, from the pie binary at the frozen snapshot with
`SOURCE_DATE_EPOCH=1757000000` and version `9.9.9`, twice:
`23710773f9be706b0a1832725ebbdb94c91d297b1d8590d8b17ea1e71590fed0`.
Taken on `08323d5` or later, so it includes the man page that
`build-deb.sh` now installs; an earlier take, before that commit, gave a
different (also stable) digest, which is the expected behaviour of a package
whose contents changed.

**It is conditioned on exactly what the Linux binary is conditioned on, and
that is the important half of the sentence.** The package contains the binary,
so everything §4.3 says about the C toolchain and the system libraries applies
unchanged. What `build-deb.sh` adds on top is the *packaging* determinism —
every mtime clamped to `SOURCE_DATE_EPOCH`, `gzip -9n` on the changelog and the
man page so no timestamp or original filename rides in the member header,
`--root-owner-group` so no uid leaks in, and `dpkg-deb` honouring the same
variable for the `ar` member headers. Stated fully:

> The `.deb` is reproducible by anyone who rebuilds it on Ubuntu 24.04 with the
> gcc, binutils, glibc, GTK3 and WebKitGTK versions recorded in the release's
> job summary, from the two commits named in `PROVENANCE.txt`, with the release
> tag's `SOURCE_DATE_EPOCH`. It is **not** "byte-identical for anyone,
> anywhere", for the same reason the binary inside it is not.

Two further facts about the interface, both confirmed by running it rather than
by reading it: `build-deb.sh` reads all five variables `.goreleaser.yaml`
passes (`DEBARK_GUI_BINARY`, `DEBARK_GUI_VERSION`, `DEBARK_GUI_ARCH`,
`DEBARK_GUI_OUTDIR`, `SOURCE_DATE_EPOCH`), and it writes exactly one `*.deb`
and fails if a second is present — which is what makes `checksum.extra_files`'
glob unambiguous, and therefore what puts the package under the signature.

---

## 5. The `replace` directive — what the release does about it

```
replace github.com/inferops/debark => ../
```

The engine — which is everything this product actually does — is read off the
parent directory of this one rather than from a module proxy. **That is the
permanent shape of this repository, not a workaround waiting on a publish.** The
CLI and this desktop application are two Go modules in one monorepo: the engine
is at the root, the application is under `gui/`, and `../` is how the second
reaches the first. It has no version, no `go.sum` entry and no `sum.golang.org`
record, and `go mod verify` cannot see it. `docs/dependency-review.md` covers
what that means for the dependency review; this section covers what it means for
a *release*.

**What the merge into one repository settled.** A release used to be built from
two repositories checked out side by side, and the hard question was *which*
`debark` a given artefact linked. `ci.yml` took the sibling at whatever `main`
was that minute, which is right for CI — noticing drift between two modules is
half of what CI is for — and wrong for a release, because an artefact built
from "`main`, that minute" is reproducible by nobody, ourselves included, and
nothing in the tarball, in `go.mod`, or in the binary recorded which commit it
was. That question no longer has two answers. **One commit describes both
halves.** The tag names a commit, that commit contains the engine and the
application, `release.yml` checks the repository out once, and `../` resolves
inside that one checkout. There is no second `actions/checkout`, no engine
commit pinned in the `Makefile`, and no post-checkout drift assertion — the
mechanism that used to close this gap was deleted because the gap closed itself.

**What still holds, and is still shipped.** The engine still has no checksum
protecting it, so the release still writes down what it was built from:
`PROVENANCE.txt`, produced by a goreleaser pre-hook and placed inside every
archive. It names the commit both halves come from, the Go version, and how to
rebuild; if the checkout was dirty it says so in the file instead of implying a
clean provenance. The release notes point at it, because a provenance file
nobody knows to look for is not provenance.

**Why a commit SHA is enough — and where it stops.** A git commit hash is
content-addressed: checking out the SHA in `PROVENANCE.txt` gets the same tree
for everyone, and cannot be substituted the way a re-pointed tag can. So a third
party genuinely can reproduce a release: clone the repository at that commit,
rebuild with the recorded toolchain, compare.

Where it stops is availability, not integrity. A `go.sum` entry is a hash the
consumer holds; a commit SHA needs `github.com/inferops/debark` to still be
there. If the repository is deleted or a branch is force-pushed past the commit,
the SHA still names the right tree but nothing can hand it to you. That gap is
accepted knowingly, and it is the one thing a module proxy would close — at the
price of un-merging the two modules, which is not on the table.

### 5.1 The pin, and why it is gone

An earlier revision of this section documented and exercised a mechanism that no
longer exists, and deleting it silently would leave the impression it was never
there. When the engine lived in a separate repository the `Makefile` carried a
40-character engine commit that the release checked the sibling out at, and it
was measured rather than asserted: the guard rejected every non-pin it was given
— branch names, short SHAs, 39 and 41 characters, uppercase hex, one non-hex
character, the empty string — the post-checkout `git rev-parse HEAD` assertion
fired in both directions, and at the time of writing the pin was nine commits
behind the engine's `main` while this GUI still built, vetted and tested green
against both ends.

**Those measurements were real and are no longer applicable.** The pin, its
guard, its assertion and the staleness they tracked all existed to answer "which
engine did this artefact link", and in one repository the tag answers it.
Nothing in them describes the current pipeline, and none of it should be read as
evidence about it.

---

## 6. Signing and provenance

Identical to `..`'s, deliberately.

- **cosign, keyless.** `debark-gui_checksums.txt` is signed with
  `cosign sign-blob` using the workflow's own OIDC token through Sigstore's
  Fulcio and Rekor. No private key exists anywhere, so there is none to leak and
  none to rotate. The certificate (`debark-gui_checksums.txt.pem`) and signature
  (`debark-gui_checksums.txt.sig`) are published alongside, named after the file
  they cover.
- **The checksums file is project-scoped, and that matters when verifying.** One
  `vX.Y.Z` tag publishes one release carrying both products, so the release page
  holds two checksums files: `debark_checksums.txt` for the CLI and
  `debark-gui_checksums.txt` for this application. There is no `checksums.txt`
  on it. Pointing `sha256sum -c` at the wrong one of the two exits 0 having
  checked nothing, which is why neither is named for goreleaser's default.
- **Everything is covered transitively.** Verify the signature on
  `debark-gui_checksums.txt`, then verify any artefact — archive, `.deb`, SBOM
  — against that file. The `.deb` is in there because `.goreleaser.yaml` adds it through
  `checksum.extra_files`; it would not be otherwise, since goreleaser did not
  build it.
- **SBOM per archive**, from syft, as SPDX JSON. One caveat recorded here rather
  than discovered later: **an SBOM of the Linux artefact describes the Go module
  graph and not the GTK3 and WebKitGTK libraries the binary dynamically links**,
  because those are system packages resolved by apt. The `.deb`'s `Depends:`
  line is where they appear.
- **SLSA build provenance** through `slsa-github-generator`'s generic generator.

### Verifying a release

```sh
cosign verify-blob debark-gui_checksums.txt \
  --signature debark-gui_checksums.txt.sig \
  --certificate debark-gui_checksums.txt.pem \
  --certificate-identity-regexp '^https://github\.com/inferops/debark/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

sha256sum -c debark-gui_checksums.txt --ignore-missing
```

The signing identity is the workflow that produced the artefacts, and that
workflow is `.github/workflows/release.yml` in `inferops/debark` — the same
one for both halves of the release, because both halves are built from this
repository. The CLI half of the same release verifies identically against
`debark_checksums.txt` and its own `.sig` and `.pem`.

---

## 7. What is pinned, and to what

| Thing | Pinned how | Where |
|---|---|---|
| `actions/checkout` | `@11bd71901bbe5b1630ceea73d27597364c9af683` (v4.2.2) | every workflow |
| `actions/setup-go` | `@40f1582b2485089dde7abd97c1529aa768e1baff` (v5.6.0) | every workflow |
| `actions/upload-artifact` | `@ea165f8d65b6e75b540449e92b4886f43607fa02` (v4.6.2) | `ci.yml` |
| `goreleaser/goreleaser-action` | `@e435ccd777264be153ace6237001ef4d979d3a7a` (v6.4.0) | `release.yml` |
| `sigstore/cosign-installer` | `@7e8b541eb2e61bf99390e1afd4be13a184e9ebc5` (v3.10.1) | `release.yml` |
| `anchore/sbom-action/download-syft` | `@43a17d6e7add2b5535efe4dcae9952337c479a93` (v0.20.11) | `release.yml` |
| goreleaser itself | `GORELEASER_VERSION := v2.18.0` | `Makefile` |
| golangci-lint | `GOLANGCI_LINT_VERSION := v2.13.2` | `Makefile` |
| Wails CLI | `WAILS_VERSION := v2.12.0` | `Makefile` |
| go-licenses | `GO_LICENSES_VERSION := v2.0.1` | `Makefile` |
| the release runner image | `runs-on: ubuntu-24.04` | `release.yml` (§4.4) |
| the Go toolchain | `go-version-file: go.mod`, `GOTOOLCHAIN: local` | every workflow |

Before this package, `ci.yml` used `actions/checkout@v4` and
`actions/setup-go@v5`. **A major tag is a mutable pointer**: whoever can
re-point it runs arbitrary code in CI with the workspace, the Go module cache
and the job token. All seven `uses:` lines in `ci.yml` are now full commit SHAs
with the release tag in a trailing comment, and the two new workflows were
written that way. Bumping a version means resolving the new tag to a commit —
editing the comment alone changes nothing, which is the point.

**Two deliberate exceptions, both stated rather than quietly taken.**

1. `slsa-framework/slsa-github-generator` is referenced by tag (`@v2.0.0`).
   It refuses to run when referenced by SHA: it inspects its own ref to put a
   trustworthy builder identity into the provenance it issues. Pinning it by SHA
   does not fail closed, it fails the job. Both provenance jobs use the same
   version on purpose, so provenance for the two halves of the release is
   issued by one builder identity.
2. The Ubuntu system libraries are not pinned at all — the paragraph below,
   stated here so the exceptions are counted in one place rather than only
   admitted afterwards.

The engine is no longer in this table because it is no longer pinned *to*
anything: it is in this repository, so the tag pins it (§5).

The Ubuntu system libraries (`libgtk-3-dev`, `libwebkit2gtk-4.1-dev`) remain
**unpinned** — apt resolves whatever the archive holds, verified by apt's own
signatures. Pinning the runner image is the mitigation; recording the resolved
versions in the job summary is the honest admission that it is a mitigation and
not a fix.

---

## 8. Cutting a release

1. Make sure `main` is green. That covers the engine too: it is in the same
   repository, so there is nothing separate to pin or confirm.
2. `make release-check` — validates `.goreleaser.yaml`.
3. `make repro` — two builds, diffed. On Linux, in the build container.
4. `make snapshot` — the whole pipeline locally, publishing nothing. Check the
   `.deb` with `packaging/verify-deb.sh` (export a `DBUS_SESSION_BUS_ADDRESS`
   first, or row 20 hangs). Two expected results, so a change in either is a
   signal rather than noise: **no `E` and no `W` from `lintian`**, and
   **19 of 20 on §5, with row 16 the one that fails** — for want of a symbol
   table on a deliberately stripped binary, not for want of the property.
   §4.7 is the whole argument.
5. Tag `vX.Y.Z` and push the tag. `release.yml` does the rest — the CLI job
   creates the release, this application's job appends to it. **A failure in
   this half leaves a published release carrying only the CLI half**; the fix
   is to re-run the `gui` job, not to re-tag.
6. Verify the published release with the commands in §6, from a clean directory.

**Use a plain `vX.Y.Z` tag.** `release.yml` also triggers on `vX.Y.Z-*`, and a
pre-release tag is a live trap: the version reaches `build-deb.sh` with a
hyphen in it, `dpkg` then reads everything after the hyphen as a Debian
revision, the package stops being native, and `lintian` reports
`E: debian-changelog-file-missing-or-wrong-name` because a non-native package
must ship `changelog.Debian.gz` and this one ships `changelog.gz`. Measured
from one binary at three versions: `1.2.3` clean, `1.2.3-rc1` and
`1.2.3-SNAPSHOT-<sha>` both drawing that `E`. It is reported to `packaging/`
rather than worked around here, because the changelog name is that script's to
choose.

Two smaller things worth knowing before the first release, both found by doing
this rather than by reading it:

- **`go install github.com/goreleaser/goreleaser/v2@v2.18.0` pulls a newer Go
  toolchain.** goreleaser v2.18.0 declares `go >= 1.27.0`, so `go install`
  switches to `go1.27.1` — which the workflows' `GOTOOLCHAIN: local` forbids.
  CI is unaffected: `goreleaser-action` downloads a release binary rather than
  building it. Only the `make release-check` / `make snapshot` instructions,
  which say `go install`, hit it.
- **`make snapshot` in a clone with no git remote silently loses its timestamp
  pinning** — §4.8's last paragraph. A normal checkout has a remote and is
  fine.

---

## 9. Open items

Recorded rather than glossed over.

Three items that used to be here are gone. Two were answered rather than
carried: the `.deb`'s reproducibility is now measured (§4.9), and the Windows
icon and DPI manifest are now a decision with a reason (§3.1) instead of a gap.
The third — a stale engine pin — stopped existing when the engine moved into
this repository (§5.1).

- **The SBOMs, and therefore `debark-gui_checksums.txt`, are not
  byte-reproducible** (§4.8). Two syft fields — a fresh UUID and a wall-clock
  stamp. Nothing else in a release moves. Not fixed, because fixing it means
  editing syft's output.
- **A pre-release tag produces a package `lintian` calls broken** (§8). Real,
  reproduced at three versions, and it belongs to `packaging/build-deb.sh`'s
  choice of changelog name. Until it is settled, cut plain `vX.Y.Z` tags.
- **`docs/security-review.md` §5 row 16 cannot corroborate a stripped binary**
  (§4.7), so the released package scores 19/20 on its own acceptance test while
  the property the row asks about does hold. Reported to whoever owns
  `packaging/verify-deb.sh`; not worked around here.
- **`packaging/verify-deb.sh` row 20 can hang forever** in a container with no
  session bus (§4.7). No timeout in the script. Export
  `DBUS_SESSION_BUS_ADDRESS` before running it.
- **The release does not gate on `lintian`.** §4.7 pins the package's clean
  bill of health to two build flags, and nothing in `release.yml` would notice
  if one of them were removed — the checklist is run by a human at §8 step 5.
  Adding `packaging/verify-deb.sh` to the release job would need `lintian`
  installed on the runner and a way to run it before `goreleaser` publishes,
  which `goreleaser release` does not offer as one command. Recorded as the
  next thing worth doing to this workflow.
- **linux/arm64 is not built** (§1).
- **Only Ubuntu 24.04 was tested for Linux reproducibility** (§4.5).
- **Signing was never executed locally** (§4.5). Keyless cosign needs an OIDC
  token, so every local run used `--skip=sign`. What it covers was produced and
  checked; the signing step itself is exercised for the first time by the first
  real release.
- **`ubuntu-latest` is still used by `ci.yml` and `licence-scan.yml`.** That is
  fine for both — CI's job is to notice drift, not to be reproducible — but it
  is a deliberate asymmetry with `release.yml` and worth knowing about.
- **`dist/` is not in `.gitignore`.** `make snapshot` and `goreleaser build`
  write there, so a local dry run leaves an untracked directory of build output
  in the tree. `.gitignore` is not this package file; the one-line fix is
  reported rather than made.
- **No `LICENSE` file question.** `.goreleaser.yaml`'s archives include
  `LICENSE`; it exists in the tree. `NOTICE` and `CHANGELOG.md`, which the core
  repository's archives carry, do not exist here and are not referenced.
