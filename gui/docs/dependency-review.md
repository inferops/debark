# Dependency review

Every dependency this application actually carries, as of
**2026-09-07T01:09Z** (tree at `46ec3ca`): what it is, why it is there, its
licence — identified by reading that module's own `LICENSE`/`COPYING` file in
the local module cache, not from a registry summary, not from `go.sum`, and
not from memory — and what its compromise would mean **for this application
specifically**.

The house format is `../docs/security/dependency-review.md`; this
document follows it, and adds two sections that repository does not need: a
build-time tool section (the definition of done in
`docs/dev/contract-brief.md` requires every build-time tool to be justified
here) and an npm section (which is one line, and it is the good kind).

**Amended in the third round, by the release work.** The tables and the
reasoning above were unchanged; five things were added or settled, and each is
marked where it appears rather than only listed here:

1. **Build-time tools gained five rows** — goreleaser, go-licenses, cosign,
   syft, and the remaining GitHub Actions — because a release pipeline is a set
   of build-time tools and the definition of done requires every one of them to
   be justified in this file. goreleaser and go-licenses are the two genuinely
   new tools this repository adopted.
2. **`actions/checkout` and `actions/setup-go` are no longer pinned to floating
   major tags.** Every `uses:` in every workflow is now a full commit SHA. Their
   rows say what they were and what they are.
3. **The Linux system libraries' licences were read**, on a Linux machine, which
   an earlier revision recorded as unfinished.
4. **A licence-scan CI job now exists**, and what it found on this tree is
   recorded in the licence summary.
5. **The `replace` directive section gained the release's answer to it** — a
   pinned engine commit, asserted in CI and shipped inside every artefact.
6. **A *Considered and declined* subsection was added** under build-time tools,
   for `go-winres`. A tool that was reported under contract rule 6 and turned
   down is part of this review's job: without the row, the next person to want
   a Windows icon re-derives the decision from nothing. `docs/release.md` §3.1
   is the decision; the row is what its compromise would have meant.

**A note on timing.** Other packages are writing into this tree right now;
`git status` at the time of writing showed in-progress edits under
`internal/catalog`. The tree compiled green (`go build ./...`, exit 0), and
`go.mod`/`go.sum` are frozen and were not touched. Re-run the commands in the
next section before every release rather than trusting this table to still be
exhaustive.

## Only what links counts

The module graph is much larger than the application. The gap is not slack, it
is one specific thing: `github.com/wailsapp/wails/v2` ships the runtime library
*and* the `wails` CLI in the same Go module, so the CLI's dependency stack
(`go-git`, `chroma`/`glamour`, `pterm`, `ghw`, `winres`, `golang.org/x/tools`
and the rest) lands in this repository's module graph without a single line of
it reaching the binary. `github.com/leaanthony/gosod` is the clearest example:
its only importer anywhere is `cmd/wails/generate.go`, the scaffolding
generator. It is in `go.mod` and in `go.sum`, and it is not in the app.

| Measure | Count | Command |
|---|---|---|
| Modules named in `go.mod` | 39 (4 direct + 35 indirect) | `cat go.mod` |
| Modules in the build list | 142 (+ the main module) | `go list -m all` |
| Distinct module paths in the module graph | 145 (198 module@version nodes, 299 edges) | `go mod graph` |
| Modules in `go.sum` | 49, of which 46 have a source-zip hash | `awk '{print $1}' go.sum \| sort -u` |
| **Linked into the shipping Linux binary** | **23 third-party modules** (132 non-stdlib packages) | `GOOS=linux go list -deps -tags webkit2_41,desktop,production .` |
| **Linked into the Windows binary** | **29 third-party modules** (162 non-stdlib packages) | `go list -deps -tags desktop,production .` |
| Union of both shipped targets | 30 third-party modules | — |
| Additionally linked by `wails dev` only | 7 more | `go list -deps -tags dev .` |

Cross-checked against a real binary. `go build -tags desktop,production` into
a scratch directory outside the repository, then `go version -m` on the
result, lists exactly 29 `dep` lines — the same 29 modules `go list -deps`
predicts for Windows. The two methods agree.

So: **142 modules resolve, 46 are downloaded, 30 are compiled, 23 ship on the
platform this product supports.** Every table below is about those, and says
which of the two targets each one is on.

## Direct dependencies

Imported by this repository's own packages. Confirmed by grepping `main.go`,
`internal/` and `hack/` for the import paths, not read off `go.mod`'s
direct/indirect markers — those lag, because packages are instructed never to run
`go mod tidy` mid-build (`docs/dev/contract-brief.md` rule 6).

| Module | Version | Licence (file read) | Used for | Where | If compromised |
|---|---|---|---|---|---|
| `github.com/wailsapp/wails/v2` | v2.12.0 | MIT — `LICENSE`, "MIT License, Copyright (c) 2018-Present Lea Anthony" | The application framework: the native window, the WebKitGTK/WebView2 host, the asset server that serves the embedded `frontend/dist`, the Go↔JS bridge, native file dialogs, and the runtime `EventsEmit` behind every progress event | `main.go`, `internal/app/bindings.go`, `internal/app/events.go` | **The single largest trust concentration in this app**, and the compromise with no floor. It sits on both sides of frozen contract 3: every argument and every return value of every bound method crosses its bridge, so it can change which packages the operator believes they selected, or the bundle path shown after a build, without touching a line of this repository's code. It serves the frontend from its own asset server, so injected script inherits full bridge access — the page loads no remote code and has no CSP of its own to stop it. On Linux it is also the cgo layer into WebKitGTK. And the same project supplies a build-time tool (see *Build-time tools*), so one MIT project with one principal maintainer owns both a runtime dependency and part of the toolchain. Nothing in the design mitigates this; it is the cost of using a webview framework, and it should be named rather than spread thinly across twelve table rows. |
| `github.com/inferops/debark` | v0.0.0 (`replace` → `../`) | Apache-2.0 — `LICENSE` plus a `NOTICE` file, read from the engine at the repository root | The engine's own types, imported and never mirrored: `api/buildjob/v1`, `core/{base,dferr,digest,evidence,lock,manifest,sign,snapshot,store,verify,version}` | `internal/cliadapter/*`, `internal/app/bindings.go`, `internal/catalog/resolve.go`, `internal/readiness/checks.go` | This *is* the product. Every claim the GUI displays — a bundle verified, a signature checked, an exit-code class turned into an actionable message — is this module's vocabulary, and the work itself is done by the `debark` binary the adapter shells out to. Compromise here is indistinguishable from compromising debark. Separately and more unusually: it is the one dependency with **no checksum protecting it at all** — see *The `replace` directive*. |
| `gopkg.in/yaml.v3` | v3.0.1 | Dual — `LICENSE` states MIT for the eight files ported from libyaml (`apic.go emitterc.go parserc.go readerc.go scannerc.go writerc.go yamlh.go yamlprivateh.go`, Kirill Simonov) and Apache-2.0 for everything else (Canonical Ltd); `NOTICE` carries the Apache header | Parsing DEP-11 application metadata (`Components-*.yml.gz`) so the picker can show human application names and freedesktop categories | `internal/catalog/dep11.go` | **The only third-party parser this repository itself points at network-fetched data.** `dep11.go` fetches many megabytes of YAML over HTTPS from whatever archive the target's own sources name, and hands it straight to this library. A malicious mirror, a compromised archive, or a memory-safety or unbounded-allocation bug in this parser is reachable after the operator has done nothing but pick a target. The blast radius is bounded by what the catalogue *is*: a browsing index that decides nothing (contract rules 1 and 2), so this cannot change what gets installed. It can change what the operator is shown, which is enough to steer a selection — a package presented under a trusted application's name and icon is a real attack on this specific product. |
| `golang.org/x/sys` | v0.47.0 | BSD-3-Clause — `LICENSE`, "Copyright 2009 The Go Authors", with the Go `PATENTS` additional-grant file alongside it | Windows volume enumeration for the export screen's drive chooser: `GetLogicalDriveStrings`, `GetDriveType`, `GetVolumeInformation`, `GetDiskFreeSpaceEx`, and `SetErrorMode` to suppress the kernel's "no disk in drive" modal | `internal/export/volumes_windows.go` (Windows only) | On the export path a compromise means the drive chooser lies: a fixed disk reported as removable, a read-only volume reported as writable, or the wrong free space — and the operator copies a bundle somewhere they did not intend. It cannot escalate into raw block-device access, because this package's use here is confined to the informational kernel32 calls a file manager makes; nothing opens `\\.\PhysicalDrive`, issues an `IOCTL_DISK_*`, or goes near diskpart, and all of that is on the permanent do-not-build list anyway. On Linux this module still links, but only for `x/sys/cpu` feature detection inside `circl` and `x/crypto` — it touches nothing this application does. |

### The two promotions, stated plainly

`golang.org/x/sys` and `gopkg.in/yaml.v3` both moved from the indirect block to
the direct block of `go.mod` during this build. **Neither added a module to the
tree.** Both were already there transitively — `x/sys` via Wails and via
`debark`'s crypto stack, `yaml.v3` via `debark` — and both were already
being compiled into the binary before anything in this repository imported
them. What changed is a marker in `go.mod` recording that this repository now
imports them directly, which is a truthful record rather than a new
dependency. Nothing was downloaded, no new maintainer was trusted, and the
module count did not move.

One correction while the subject is open: the promotion of `x/sys` is
attributable to `internal/export` alone. The readiness disk check on Windows
deliberately does **not** use it — `internal/readiness/checks_windows.go` calls
`GetDiskFreeSpaceExW` through stdlib `syscall.NewLazyDLL` precisely to avoid
making `x/sys` direct, and its comment saying `x/sys` "is present only as an
indirect dependency" is now stale rather than wrong-in-spirit.

## Transitive dependencies that actually link

Not imported by any package in this repository (confirmed by grep); present
because something above needs them. Go compiles only what is imported, so
`go.sum`'s other sixteen modules do not appear here and do not affect the
binary.

**Both platforms:**

| Module | Version | Licence (file read) | Pulled in by | Note / if compromised |
|---|---|---|---|---|
| `github.com/ProtonMail/go-crypto` | v1.4.1 | BSD-3-Clause — `LICENSE` is the Go Authors text ("Neither the name of Google Inc."); `PATENTS` present | `debark`, `pault.ag/go/debian` | OpenPGP parsing during snapshot keyring enumeration. As in the core repository, it makes **no trust decision**: the GUI verifies nothing in-process, `Verify` is `debark verify --json` in a separate process. Worst case is wrong or incomplete fingerprints shown in a snapshot summary — an audit/correctness problem, not a verification bypass. 23 of its packages link. |
| `github.com/cloudflare/circl` | v1.6.2 | BSD-3-Clause — `LICENSE` carries two BSD-3-Clause blocks: Cloudflare 2019, then a second Go Authors block after a `====` separator | `ProtonMail/go-crypto` | Elliptic-curve and post-quantum primitives behind the OpenPGP parsing above. Same bounded consequence. |
| `golang.org/x/crypto` | v0.41.0 | BSD-3-Clause — `LICENSE` + `PATENTS` | `ProtonMail/go-crypto`, `circl` | argon2, blake2b, cast5, cryptobyte, hkdf, sha3. Reached only through the OpenPGP path. |
| `github.com/gowebpki/jcs` | v1.0.1 | Apache-2.0 — full Apache text in `LICENSE` | `debark` | RFC 8785 canonicalisation. In the core repository this is the highest-leverage dependency in the tree; in *this* repository it is one step removed, because the digests the GUI shows come back from the CLI subprocess rather than being computed here. It still links, so any in-process digest work goes through it. |
| `github.com/klauspost/compress` | v1.18.0 | BSD-3-Clause — `LICENSE` (Go Authors 2012 + Klaus Post 2019) with per-directory exceptions listed further down the same file: `gzhttp/*` Apache-2.0, `s2/cmd/internal/readahead/*` MIT, `snappy/*` and `internal/snapref/*` BSD-3-Clause (Snappy-Go Authors), `s2/cmd/internal/filepathx/*` MIT. Only `zstd`, `fse`, `huff0`, `internal/snapref` and their helpers link here — all BSD-3-Clause | `debark`, `pault.ag/go/debian` | zstd decompression of a snapshot the operator chose from disk. **Untrusted input, and the exact input this product exists to move**: a snapshot arrives on a USB stick from someone else. A memory-safety bug in the decompressor is code execution in the GUI process on the builder machine — the online machine, with credentials on it. The dependency most worth watching for CVEs in this tree. |
| `github.com/xi2/xz` | (pseudo-version, 2017) | Public domain — its own `LICENSE` says so explicitly: a modified XZ Embedded, "All these files have been put into the public domain." No compatibility question with Apache-2.0 | `pault.ag/go/debian` | XZ decompression of `.deb` members. Same untrusted-input position as zstd, and unmaintained since 2017 — a pseudo-version with no upstream releases is a standing risk, not a new one. |
| `github.com/kjk/lzma` | (pseudo-version, 2016) | BSD-3-Clause — `LICENSE`, "Copyright (c) 2010, Andrei Vieru", written as Go comment lines rather than plain text | `pault.ag/go/debian` | Legacy LZMA for older `.deb` formats. Same note as `xi2/xz`: untrusted input, unmaintained. |
| `pault.ag/go/debian` | v0.21.0 | MIT for the bulk — `LICENSE`, "Copyright (c) Paul R. Tagliamonte, 2015-2016" — plus a second, BSD-3-Clause section headed *Version module*, "Copyright © 2012 Michael Stapelberg and contributors" | `debark` | `.deb` and control-stanza parsing; on the untrusted-input path for every vendor `.deb` a build touches. Worth flagging explicitly: its `version` subpackage **is** a Debian version comparator and **is** linked into this binary. Nothing in this repository calls it — contract rule 1 forbids comparing two version strings anywhere in this app, and grep confirms no import. Anyone auditing the shipped binary will find a version comparator inside it; this row is why. |
| `pault.ag/go/topsort` | v0.1.1 | MIT — `LICENSE`, "Copyright (c) Paul R. Tagliamonte, 2015" | `pault.ag/go/debian` | Topological sort used inside `pault.ag/go/debian`'s own relationship modelling. **Not a dependency solver, and never called from here** — flagged for the same reason the core repository flags it: out of context the name looks exactly like the "pure-Go apt dependency solver" the permanent do-not-build list forbids. It is not one. |
| `github.com/pkg/errors` | v0.9.1 | BSD-2-Clause — `LICENSE`, "Copyright (c) 2015, Dave Cheney"; two conditions only, no non-endorsement clause | Wails | Error wrapping inside Wails. Archived upstream; no security surface of its own. |
| `github.com/pkg/browser` | (pseudo-version, 2024) | BSD-2-Clause — `LICENSE`, "Copyright (c) 2014, Dave Cheney" | Wails | Opens a URL in the operator's browser. Small, but a library whose entire job is to launch another program — on Linux via `xdg-open`. A compromise turns "open a link" into "run a command" on the builder machine. |
| `github.com/leaanthony/go-ansi-parser` | v1.6.1 | MIT — `LICENSE`, "Copyright (c) 2021-Present Lea Anthony" | Wails | ANSI parsing for Wails' logger. |
| `github.com/rivo/uniseg` | v0.4.7 | MIT — `LICENSE.txt`, "Copyright (c) 2019 Oliver Kuederle" | `go-ansi-parser` | Unicode grapheme segmentation, for width calculation in the above. |
| `github.com/leaanthony/slicer` | v1.6.0 | MIT — `LICENSE`, "Copyright (c) 2019 Lea Anthony" | Wails | Slice helpers. |
| `github.com/leaanthony/u` | v1.1.1 | MIT — `LICENSE`, "Copyright (c) 2023-Present Lea Anthony" | Wails | Small utility helpers. |
| `github.com/samber/lo` | v1.49.1 | MIT — `LICENSE`, "Copyright (c) 2022-2025 Samuel Berthe" | Wails | Generic slice/map helpers. |
| `golang.org/x/text` | v0.28.0 | BSD-3-Clause — `LICENSE` + `PATENTS` | `samber/lo` | Case folding and normalisation, reached only through `lo`. |
| `github.com/tkrajina/go-reflector` | v0.5.8 | Apache-2.0 — `LICENSE.txt`, full Apache text | Wails | **Deserves a sentence of its own.** This is what Wails' binding layer reflects over the bound `App` methods with. It sees the type and value of every argument and every return crossing frozen contract 3. Low profile, high position. |

**Linux only:**

| Module | Version | Licence (file read) | Pulled in by | Note / if compromised |
|---|---|---|---|---|
| `github.com/godbus/dbus/v5` | v5.1.0 | BSD-2-Clause — `LICENSE`, "Copyright (c) 2013, Georg Reinke, Google"; two conditions, no non-endorsement clause | Wails | D-Bus, for desktop notifications and session integration on Linux. A compromise reaches the operator's session bus — the same bus their password manager and screen locker are on. |

**Windows only** (built and tested, not a supported target — but these ship in
any Windows build a developer produces):

| Module | Version | Licence (file read) | Pulled in by | Note |
|---|---|---|---|---|
| `github.com/wailsapp/go-webview2` | v1.0.22 | MIT — `LICENSE`, "Copyright (c) 2020 John Chadwick; Some portions Copyright (c) 2017 Serge Zaitsev" | Wails | Loads and drives the Edge WebView2 runtime. Compromise = control of the renderer, i.e. everything the operator sees and clicks. |
| `github.com/go-ole/go-ole` | v1.3.0 | MIT — `LICENSE`, "Copyright © 2013-2017 Yasuhiro Matsumoto" | Wails, `go-toast` | COM interop. |
| `git.sr.ht/~jackmordaunt/go-toast/v2` | v2.0.3 | Dual, explicit — `LICENSE` opens with `SPDX-License-Identifier: Unlicense OR MIT` and reproduces both texts in full | Wails | Windows toast notifications. |
| `github.com/wailsapp/mimetype` | v1.4.1 | MIT — `LICENSE`, "Copyright (c) 2018-2020 Gabriel Vasile" | Wails | Content-type sniffing for the asset server. Sits on the path that decides how the webview interprets a served file. |
| `golang.org/x/net` | v0.42.0 | BSD-3-Clause — `LICENSE` + `PATENTS` | `mimetype`, Wails | `html` and `html/atom` only — HTML tokenisation for sniffing. No networking code links. |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause — `LICENSE`, "Copyright (c) 2009,2014 Google Inc." | Wails | Identifiers inside Wails. |
| `github.com/bep/debounce` | v1.2.1 | MIT — `LICENSE`, "Copyright (c) 2016 Bjørn Erik Pedersen" | Wails | Debouncing window events. |

## Downloaded but never compiled

Present in `go.sum` with a source hash — so they are fetched and verified —
and linked by nothing under any of the four `GOOS` × build-tag combinations
tested:

- `github.com/jchv/go-winloader` — reached only from
  `go-webview2/webviewloader/native_module.go`, guarded
  `//go:build windows && native_webview2loader`. This project never sets that
  tag.
- `github.com/leaanthony/gosod` — imported only by `cmd/wails/generate.go`,
  the Wails CLI's scaffolding generator.
- `github.com/stretchr/testify`, `github.com/davecgh/go-spew`,
  `github.com/pmezard/go-difflib`, `github.com/matryer/is`,
  `gopkg.in/check.v1`, `github.com/kr/text`, `github.com/niemeyer/pretty`,
  `github.com/leaanthony/debme` — test and template dependencies of other
  modules. **This repository's own test suite uses stdlib `testing` and
  nothing else**: `go list -deps -test ./...` adds not one module beyond the
  linked set, and grep finds no assertion library imported anywhere in the
  tree.

They still matter a little: they are downloaded, so a compromised release of
one of them is code sitting in the module cache of every developer and every CI
runner. It is just not code in the product.

## Linked by `wails dev` only

`wails dev` compiles with the `dev` tag, which switches on Wails' live-reload
dev server. That pulls in seven more modules that no shipped build contains.
Developers run this daily, so they are reviewed rather than ignored — all seven
are permissive, licences read from their own files in the module cache:

`github.com/labstack/echo/v4` v4.13.3 (MIT, "Copyright (c) 2021 LabStack"),
`github.com/labstack/gommon` v0.4.2 (MIT, "Copyright (c) 2018 labstack"),
`github.com/gorilla/websocket` v1.5.3 (BSD-2-Clause, "The Gorilla WebSocket
Authors"), `github.com/mattn/go-colorable` v0.1.13 (MIT),
`github.com/mattn/go-isatty` v0.0.20 (MIT/Expat),
`github.com/valyala/bytebufferpool` v1.0.0 (MIT),
`github.com/valyala/fasttemplate` v1.2.2 (MIT).

What they are, and why the split matters: `echo` plus `gorilla/websocket` is an
HTTP server and a websocket endpoint bound on the developer's machine while
`wails dev` runs. That is the only configuration in which this application
listens on a socket at all. It is a development-time attack surface on a
builder machine, and it is absent from every shipped binary — which is exactly
why the table at the top of this document distinguishes the two.

## The `replace` directive — a real supply-chain fact

```
replace github.com/inferops/debark => ../
```

`debark` is the other half of this repository — the engine at the root, this
application under `gui/` — and it is **consumed by path, not from a module
proxy. That is the permanent arrangement for this layout, not a temporary
workaround.** Everything the product does comes from it, and it is the one
dependency here with no checksum. Concretely:

- **No `go.sum` entry, no `sum.golang.org` verification, no version.** Every
  other module in this document is fetched through `proxy.golang.org` and
  checked against a hash committed to this repository. This one is read off
  the local disk.
- **A built binary does not record which `debark` it was built against.**
  `go version -m` on the probe binary prints
  `dep github.com/inferops/debark v0.0.0 => .. (devel)` with no
  `h1:` hash. There is nothing to compare, so no reproducible-build or
  provenance claim can cover the engine half of this application.
- **A checkout of `gui/` alone does not build.** The engine has to be there
  too. In this repository it always is — one `actions/checkout` gets both
  halves, and there is no second checkout and no engine ref to pin — but a
  copy of this directory taken out of the repository will not compile.
- **The path `../` is trusted implicitly.** Anything that can write to the
  repository root changes what this application is, silently, with no hash
  mismatch anywhere to notice. Inside one repository that is the same trust a
  build already places in its own source tree; it is listed because the
  *mechanism* differs from every other row in this document.

None of this is a defect: it is the arithmetic of two Go modules in one
repository, and there is no exit plan because there is nothing to exit. An
earlier revision of this document described one — tag `debark`, publish it to
the proxy, turn the directive into a `require`, delete the sibling checkout —
and that plan is obsolete: the sibling checkout is already gone, and publishing
the engine to a proxy would mean splitting the repository back into two, which
is not the direction. It belongs in this document because a dependency review
that omitted the one unhashed dependency would be worthless.

**What the release does about it.** When the engine lived in a separate
repository this was the sharp edge: an artefact built from whatever the
sibling's `main` was that minute could not be rebuilt by anyone, including us,
so `.github/workflows/release.yml` pinned the engine to a 40-character commit,
checked the sibling out at it, and asserted `git rev-parse HEAD` matched rather
than trusting that it did. **None of that machinery exists any more, and none of
it is needed:** one repository means one commit describes both halves, and the
tag is the pin. What survives is the record — a `PROVENANCE.txt` shipped inside
every archive naming the commit both halves were built from, with the release
notes pointing at it.

A git commit hash is content-addressed, so this restores the property that
mattered: a third party can check the repository out at the commit named
in the artefact and rebuild it. What it does not restore is *availability* — a
`go.sum` hash is held by the consumer, whereas a commit SHA needs the upstream
repository to still be there. That gap is knowingly accepted for a bounded time.
`docs/release.md` §5 is the long version, and the second bullet above — "a built
binary does not record which `debark` it was built against" — is now false for
a released artefact and still true for every other build.

## npm dependencies: there are none

Zero. Not "zero runtime dependencies with a build-time tree behind them" —
zero npm dependencies of any kind, because there is no bundler, no transpiler
and no JavaScript build step. Verified mechanically, not asserted:

- `find . -name package.json` returns exactly one tracked file:
  `frontend/wailsjs/runtime/package.json`. It is the Wails JS runtime's own
  vendored metadata, generated by the Wails CLI and committed. It declares no
  `dependencies`, no `devDependencies`, and an empty `scripts` block. It is
  MIT (its own `license` field, and the module it was generated from is MIT by
  the `LICENSE` read above). Nothing ever installs from it.
- No `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `bun.lockb`,
  `node_modules`, `.npmrc`, `vite.config.*`, `webpack.config.*`,
  `rollup.config.*` or `tsconfig.json` anywhere in the tree, tracked or
  untracked.
- Every `import` in `frontend/src/` is a relative path — grep for an import
  from a bare specifier or a URL returns nothing.
- No `<script src="https://…">` and no remote stylesheet: grep for `src=` or
  `href=` with an `http` scheme across `frontend/` returns nothing. The page
  loads only files that came out of this repository.
- `wails.json` sets `"frontend:install": ""`, so the Wails CLI runs no package
  manager at all, and `"frontend:build": "go run ../hack/copyfrontend"`, which
  is a recursive file copy.

That is contract rule 5 satisfied by construction rather than by policy: there
is no `npm install` to forget to review, because there is nothing to install.

## Build-time tools

These never link into the binary. They can all compromise it anyway, which is
why the definition of done asks for them here. They are kept separate from the
tables above on purpose: a linked dependency and a tool that produces the
linked binary fail in different ways and are mitigated differently.

| Tool | Version / pin | Licence (file read) | What it does here | If compromised |
|---|---|---|---|---|
| Go toolchain | go1.26.0 (`go.mod`; CI sets `GOTOOLCHAIN=local` and uses what `actions/setup-go` installed from the `go` line) | BSD-3-Clause — read from `$(go env GOROOT)/LICENSE`, "Copyright 2009 The Go Authors", with `PATENTS` alongside | Compiles and links everything; `gofmt` is part of it, and `go vet` runs from it | The trusting-trust position: total, and undetectable from inside the source. Worth knowing that on this development machine `GOROOT` is itself a *downloaded module* — `golang.org/toolchain@v0.0.1-go1.26.0.windows-amd64` in the module cache — so the compiler arrives over the same channel as the dependencies, verified by the same checksum database. CI's `GOTOOLCHAIN=local` deliberately avoids that second fetch. |
| Wails CLI | v2.12.0, pinned in the `Makefile` (`WAILS_VERSION`), installed by CI with `go install …/cmd/wails@$WAILS_VERSION` | MIT — the same `LICENSE` as the runtime; the CLI and the library are one module | Generates `frontend/wailsjs/` bindings, runs `wails.json`'s `frontend:build`, embeds the assets, and performs the final platform link | The highest-value build-time target. It writes the generated bindings, which are **committed and shipped inside the binary**, and it controls the link step, so a compromised CLI can put code in the app that appears in no source file anyone reviews. It is also the reason the module graph is 142 modules deep: its own dependency stack (`go-git`, `chroma`, `pterm`, `ghw`, `winres`, `x/tools`) is downloaded and verified on every `go install`, and none of it reaches the product. |
| golangci-lint | v2.13.2, pinned in the `Makefile`, version asserted by CI after install | **GPL-3.0** — read from `…/golangci-lint/v2@v2.13.2/LICENSE`, "GNU GENERAL PUBLIC LICENSE Version 3, 29 June 2007" | Runs `.golangci.yml` over the tree; `make check` and CI both gate on it | **The only copyleft licence anywhere in this review**, and it is worth writing down rather than discovering later. It never links into anything: it is a separate binary run *over* source, the same relationship this project has with `make` or `bash`, so it places no obligation on the application's own licence — the same reasoning the core repository applies to `apt`, `dpkg` and `gpg`. As an attack it is quiet and effective: it runs with full read access to the source tree and to CI's credentials, and its silence is trusted, so a linter compromised to *not* report an introduced bug leaves no trace. The core repository pins the same version deliberately, so a finding does not depend on which of the two modules you are standing in. |
| `hack/copyfrontend` | in-repo, stdlib only (`errors flag fmt io io/fs os path/filepath strings`) | This repository's own code | **This is the entire frontend build.** It copies `frontend/index.html`, `frontend/src/` and `frontend/wailsjs/` into `frontend/dist/`, which `main.go` embeds | Anything that can edit this file can write arbitrary content into the embedded asset bundle — the cheapest route to getting script into the shipped app, since `frontend/dist` is gitignored and an injected file therefore appears in no diff. CI asserts the output *exists* (`frontend/dist/index.html` present, file count printed); it does not assert what is in it. That gap is the honest reading of this row. |
| `hack/fetch-indexes.go` | in-repo, `//go:build ignore`, stdlib only | This repository's own code | Downloads real apt indexes to a directory **outside** the repository so the catalogue packages can measure the performance budgets against real data | Never compiled into anything — that is what the `ignore` tag is for — and it writes nothing into the tree. It does make network requests, to distro archives only, which is the same host set contract rule 3 permits. |
| goreleaser | v2.18.0, pinned in the `Makefile` (`GORELEASER_VERSION`), read back out of it by `release.yml` with `sed` | MIT — read from `…/goreleaser/v2@v2.18.0/LICENSE.md` in the module cache, "Copyright (c) 2016-2026 Carlos Alexandro Becker" | **New build-time tool for this repository, adopted in this round.** Drives the whole release: builds both binaries with the release flags, calls `packaging/build-deb.sh`, writes the archives and `debark-gui_checksums.txt`, invokes cosign and syft, and appends to the GitHub release the CLI's own goreleaser run created for the same tag. Never linked into anything. See `docs/release.md` | Precedented — the core repository has used it since its own first release, and matching its practice rather than inventing a second convention was the explicit instruction. That does not make it free. A compromised goreleaser controls the compiler invocation for every published artefact, and it does so at the one moment nobody is watching the output: it can pass different flags, different ldflags or a different package than the config says, and the config is what anyone reviews. Its own dependency stack is very large (cosign, sigstore, the AWS/Azure/GCP KMS clients, go-git) and every bit of it is trusted at release time with the `GITHUB_TOKEN` and the OIDC token that mints the signature. Mitigations actually in place: the version is pinned rather than a `~> v2` range, `hack/reproducible-check.sh` builds the same targets independently of goreleaser with the same flags and fails the release if the two builds differ, and the release binary is checked to actually start. None of that would catch a goreleaser that lied consistently. |
| `go-licenses` | v2.0.1, pinned in the `Makefile` (`GO_LICENSES_VERSION`), read back out by `licence-scan.yml` | Apache-2.0 — read from `…/go-licenses/v2@v2.0.1/LICENSE` in the module cache (the stock Apache-2.0 text; the module carries no separate copyright line or `NOTICE`) | **New build-time tool for this repository, adopted in this round.** `.github/workflows/licence-scan.yml` runs `go-licenses report` and `go-licenses check --disallowed_types=forbidden,restricted` over the tree with the shipping build tags | Read-only over source and the module cache; it produces no artefact. Its failure mode is the linter's, in reverse: it is trusted to *say no*, so a compromised or merely wrong classifier that reports a copyleft dependency as permissive leaves no trace at all. That is why this hand-written document still exists and is not replaced by the job — the job is a ratchet on this document, not a substitute for it. Pinned here where the core repository installs it with `@latest`: the tool that decides whether this project's licensing is acceptable must not change between two runs with no diff anywhere. |
| cosign (`sigstore/cosign-installer`) | action `@7e8b541eb2e61bf99390e1afd4be13a184e9ebc5` (v3.10.1); the cosign binary is whatever that action installs | Apache-2.0 (cosign's own licence; **not read here** — the binary is installed in CI and is not present on this machine) | Signs `debark-gui_checksums.txt` in keyless mode through the workflow's OIDC token (Sigstore Fulcio/Rekor) | It is the signature. A compromised cosign can sign whatever it likes with a certificate that genuinely names this workflow, and the resulting signature verifies. There is no private key to steal, which removes one whole class of failure and adds this one. |
| syft (`anchore/sbom-action/download-syft`) | action `@43a17d6e7add2b5535efe4dcae9952337c479a93` (v0.20.11) | Apache-2.0 (syft's own licence; **not read here** — installed in CI) | Produces an SPDX SBOM per archive | Writes a document *about* the artefacts, never the artefacts, so a compromise is a lie in the supply-chain record rather than in the product. Worth knowing that the SBOM it emits for the Linux archive covers the Go module graph and **not** GTK3 or WebKitGTK, which are dynamically linked system packages it cannot see. |
| `actions/checkout` | `@11bd71901bbe5b1630ceea73d27597364c9af683` (v4.2.2) | Not read — GitHub Actions are not present on this machine, so there was no licence file to read. Stated rather than guessed | Checks out this repository and the sibling `debark` | **Now pinned to a full commit SHA** (it was `@v4`, a floating major tag, until this round). A major tag is a mutable pointer: re-pointing it ran arbitrary code in CI with the workspace, the Go module cache and the job token. The trailing comment is for humans; the SHA is the pin. The residual exposure is the action's own transitive JavaScript, which is not reviewed here and would not be reviewable without vendoring it. |
| `actions/setup-go` | `@40f1582b2485089dde7abd97c1529aa768e1baff` (v5.6.0) | Not read — same reason | Installs the Go toolchain named by `go.mod` and restores the module cache keyed on `go.sum` | Same, on the step that supplies the compiler, which makes it the highest-value of the three. Was `@v5`; now SHA-pinned. |
| `actions/upload-artifact` | `@ea165f8d65b6e75b540449e92b4886f43607fa02` (v4.6.2) | Not read — same reason | Uploads the end-to-end harness report from `ci.yml`'s `e2e` job | Touches no released artefact. Was `@v4`; now SHA-pinned for consistency, so that "every action in this repository is SHA-pinned" is a statement someone can check with one grep rather than a rule with an exception. |
| `goreleaser/goreleaser-action` | `@e435ccd777264be153ace6237001ef4d979d3a7a` (v6.4.0) | Not read — same reason | Installs the pinned goreleaser and runs it | A wrapper around the goreleaser row above; it decides *which* goreleaser runs. It is handed `version: ${{ env.GORELEASER_VERSION }}` from the `Makefile` rather than a range, so a compromise here means substituting the tool, not selecting a different version of it. |
| `slsa-framework/slsa-github-generator` | `@v2.0.0` — **a tag, not a SHA** | Not read — same reason | Issues SLSA build provenance for the published artefacts | The one deliberate unpinned reference in this repository, and it is deliberate because pinning it does not work: the generator inspects its own ref to put a trustworthy builder identity into the provenance it issues and refuses to run when referenced by SHA. Recorded in `docs/release.md` §7 rather than left for someone to find with a grep and assume was an oversight. It produces provenance and touches no artefact, so a compromise is a false attestation rather than a bad binary — which is still bad, because attestation is the thing people trust when they cannot rebuild. |
| `libgtk-3-dev`, `libwebkit2gtk-4.1-dev` | whatever `apt-get install` resolves on the runner — `ubuntu-latest` in `ci.yml`, pinned `ubuntu-24.04` in `release.yml` | **Not read.** These are Ubuntu system packages; their licence files are not on this (Windows) machine, and this review did not guess | Linux build dependencies — cgo compiles and links against them, so even `go build ./...` does not work on Linux without them | The renderer and the toolkit: a compromise is everything the operator sees. Unlike every Go module here they are **unpinned** — CI installs whatever the archive currently holds, verified by apt's own signatures rather than by anything in this repository. Their licences must be confirmed on a Linux machine before release; see the open items. |

### Considered and declined

A tool that was reported and *not* adopted belongs here as much as one that
was, because otherwise the next person to hit the same gap re-derives the
decision from scratch — and because "we chose not to take this dependency" is
only credible if the reasoning was written down at the time.

| Tool | What it would do here | Why not | What its compromise would have meant |
|---|---|---|---|
| `go-winres` | Compile `build/windows/icon.ico` and `build/windows/info.json` into a `.syso`, giving the released Windows executable its icon and its DPI-awareness manifest. `wails build` already does exactly this; goreleaser cannot. | Declined. Windows is built, not supported, and this would be the first dependency this project took on for cosmetics. Nothing here can start a Windows GUI binary to confirm the result, so it would be an unverifiable change to an unsupported artefact. `docs/release.md` §3.1 is the decision and what would reverse it. | It emits an **object file that is linked into the shipped binary** — not a document about it, not a check over it. That puts it in the same class as the Wails CLI and above every other tool in the table but the compiler: a compromised generator can place code in the executable that appears in no source file anyone reviews, and a `.syso` is precisely the artefact nobody reads. The committed-`.syso` alternative is worse still: the same opacity, permanently, with nothing in the tree able to regenerate it for comparison. |

## No telemetry, no analytics, no crash reporting

Checked mechanically, twice. `go.sum` (110 lines) and the full list of linked
package import paths were both searched for `analytics`, `telemetry`,
`sentry`, `bugsnag`, `segment`, `mixpanel`, `amplitude`, `rollbar`,
`datadog`, `newrelic`, `honeycomb`, `posthog`, `crashlytics`, `raygun`,
`honeybadger`, `opentelemetry`, `otel`, `statsd`, `prometheus`. Zero matches in
either.

Every linked module above is a data-format library, a compression library, a
crypto primitive, a Win32/D-Bus binding, or part of Wails. This is contract
rule 4 as an architectural property — nothing to disable, because there is
nothing there — and it is checkable by anyone with `go.sum` and five minutes,
which is the point.

One networking observation belongs alongside it: the only linked code that
opens an outbound connection is `net/http` in `internal/catalog/packages.go`
and `internal/catalog/dep11.go`, reaching the archives named in the target's
own sources. No linked module provides an HTTP *server* in a shipped build;
`echo` and `gorilla/websocket` appear only under the `dev` tag.

## Licence summary

Everything linked into a shipped binary is Apache-2.0, MIT, BSD-2-Clause,
BSD-3-Clause, Unlicense-or-MIT, or public domain. **No copyleft licence
(GPL/AGPL/LGPL/MPL family) is linked into this application.** That is
consistent with `debark`'s own Apache-2.0 licence and with the core
repository's `licence-scan.yml` policy.

Two caveats, both stated because a summary that hid them would be dishonest:

1. **golangci-lint is GPL-3.0.** Build-time only, executed as a separate
   program, never linked — the standard, well-established boundary that keeps
   its licence from attaching. It is nonetheless the one copyleft licence in
   this project's toolchain, and it should not surprise anyone reading a
   licence report later.
2. **The Linux system libraries are LGPL-family, and that is fine.** An earlier
   revision of this document could not read their licences from a Windows
   machine. They were read in this round, on Ubuntu 24.04.4 inside the build
   container, from each package's own
   `/usr/share/doc/<pkg>/copyright`:

   | Package | Version | `Files: *` licence |
   |---|---|---|
   | `libgtk-3-0t64` | 3.24.41-4ubuntu1.3 | LGPL-2+ and LGPL-2.1+ and Expat |
   | `libwebkit2gtk-4.1-0` | 2.52.6-0ubuntu0.24.04.1 | BSD-2-clause |
   | `libjavascriptcoregtk-4.1-0` | 2.52.6-0ubuntu0.24.04.1 | BSD-2-clause |
   | `libc6` | 2.39-0ubuntu8.8 | LGPL-2.1+ (glibc) |

   So the shipped Linux binary **does** link copyleft code — LGPL-family, in
   GTK3 and glibc — and that is the ordinary, intended arrangement rather than a
   finding. It is *dynamic* linking against the copies the operator's own
   distribution installed, which is exactly what an LGPL library is for and what
   a `.deb` with a `Depends:` line expresses. It places no obligation on this
   application's own Apache-2.0 licence beyond the LGPL's relinking allowance,
   which dynamic linking already satisfies. What *would* be a regression is
   vendoring or statically linking them, and that would need its own review
   rather than a green tick from any job.

   The WebKitGTK copyright file is 4,894 lines and names dozens of licences
   across its many upstream components (much of WebCore is LGPL-2.1+); the row
   above is the `Files: *` stanza, which is what governs the library as a whole.
   One `GPL-3+` stanza appears in GTK3's copyright file and covers exactly one
   file, `debian/tests/run-with-display` — a packaging test script that is not in
   the shipped library and is not linked by anything.

Note also that this repository now has a **licence-scan CI job**, which it
lacked when this document was first written:
[`.github/workflows/licence-scan.yml`](../../.github/workflows/licence-scan.yml),
which lives at the repository root and carries a dedicated job for this module
alongside the engine's.
It runs `go-licenses report` and `go-licenses check ./...
--disallowed_types=forbidden,restricted` on every `go.mod`/`go.sum`/`Makefile`
change, weekly, and on demand.

**What it found on this tree**, run with go-licenses v2.0.1 and the shipping
Linux build tags (`desktop,production,webkit2_41`): **`check` exits 0 — no
forbidden or restricted licence.** 32 rows in the report: 12 MIT, 10
BSD-3-Clause, 6 Apache-2.0, 3 BSD-2-Clause, and one classified `XZ`
(`github.com/xi2/xz`, a public-domain-equivalent notice licence, neither
forbidden nor restricted). That agrees with the tables above, which is the
result this document wanted: the job is a ratchet on a hand-written review, not
a replacement for one.

Two details of that run are worth writing down because they will recur:

- `github.com/inferops/debark`'s row reads `Unknown` in the source-URL column.
  go-licenses reads Apache-2.0 correctly from the engine's own
  `LICENSE`, but cannot produce a URL for a module resolved by a filesystem
  `replace` directive — it tries to resolve `..` as a hostname. That is
  the replace directive showing through again, not a licence problem.
- go-licenses does **not** see GTK3, WebKitGTK or glibc, and should not: they
  are not Go imports and appear in no `go.mod` or `go.sum`. The table above is
  the only place in this repository their licences are recorded. This is the
  same boundary the core repository draws around `apt` and `dpkg`.

## What this review could not settle

Recorded rather than glossed over:

- **GTK3 and WebKitGTK licences: now settled.** They were read on Ubuntu
  24.04.4 in the build container; see the licence summary above for the table
  and for why LGPL dynamic linking is the intended arrangement rather than a
  finding. What is still open is narrower: those versions are **what
  `ubuntu-latest` resolved on one day**, and nothing pins them. A different
  Ubuntu or Debian release ships different versions with different copyright
  files, and the release job records the resolved versions per build rather than
  pretending they are fixed (`docs/release.md` §4.4).
- **No GitHub Action's licence was read** — `actions/checkout`,
  `actions/setup-go`, `actions/upload-artifact`, `goreleaser/goreleaser-action`,
  `sigstore/cosign-installer`, `anchore/sbom-action`,
  `slsa-framework/slsa-github-generator`. They are not present on this machine,
  and guessing was not worth it. Their compromise scenarios, which are the part
  that matters, are recorded above; so are the commit SHAs they are now pinned
  to, which is the part that is actionable.
- **cosign's and syft's own licences were not read either**, for the same
  reason: they are installed inside a CI runner and exist nowhere locally. Both
  are Apache-2.0 by their projects' published terms, which is a claim from
  memory and is marked as such rather than presented as a file that was read.
- **The `.deb` packaging's own runtime dependencies are not covered here.**
  This document covers what the Go build links and what the build uses. The
  shipped `.deb`'s `Depends:` line — which will name the WebKitGTK and GTK
  runtimes — is a packaging question and belongs with whoever owns it.
- **A `LICENSE` file now exists** at the repository root — a full Apache-2.0
  text, matching what `README.md` has always claimed and what `.goreleaser.yaml`
  ships inside every release archive. An earlier revision of this document
  reported it as missing. `NOTICE` and `CHANGELOG.md` — which the core
  repository's release archives carry — still do not exist here, and this
  repository's archives do not reference them.
- **`go mod tidy` was not run**, by rule (`docs/dev/contract-brief.md` rule 6 —
  running it mid-round prunes `require github.com/inferops/debark` and has
  broken the tree twice). So `go.mod`'s direct/indirect markers were not
  re-derived. Every direct/indirect claim above comes from grep over the source
  and from `go list -deps`, neither of which depends on tidy having been run.

## Reproducing this

```sh
# What actually links, per shipping target.
GOOS=linux  go list -deps -tags webkit2_41,desktop,production \
    -f '{{if .Module}}{{.Module.Path}} {{.Module.Version}}{{end}}' . | sort -u
GOOS=windows go list -deps -tags desktop,production \
    -f '{{if .Module}}{{.Module.Path}} {{.Module.Version}}{{end}}' . | sort -u

# The whole graph, for the difference.
go list -m all | wc -l
go mod graph | wc -l

# Cross-check against a real binary — build OUTSIDE the repository.
go build -tags desktop,production -o /tmp/probe .
go version -m /tmp/probe | grep '^	dep'

# Licences, read from each module's own file in the module cache.
go list -deps -f '{{if .Module}}{{.Module.Dir}}{{end}}' . | sort -u | \
    xargs -I{} sh -c 'ls {}/LICENSE* {}/COPYING* 2>/dev/null'

# The mechanical version of the two tables, which is what CI runs. Needs the
# shipping build tags, or it scans a different package set from the one that
# links. Linux only: go-licenses runs the type checker and this tree is cgo.
make licences
#   == go-licenses report ./...  and
#      go-licenses check ./... --disallowed_types=forbidden,restricted
#      with GOFLAGS=-tags=desktop,production,webkit2_41

# The system libraries go-licenses cannot see, on a Linux machine.
for p in libgtk-3-0t64 libwebkit2gtk-4.1-0 libjavascriptcoregtk-4.1-0 libc6; do
    awk '/^Files: \*/{f=1} f&&/^License:/{print FILENAME": "$0; exit}' \
        "/usr/share/doc/$p/copyright"
done
```

All read-only. None of them mutates `go.mod`, `go.sum` or the module cache.
