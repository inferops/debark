# Windows — what was run, what it produced, and what is not promised

## The honest headline: Windows is built and tested, and it is not supported

Debark compiles for `windows/amd64`, the desktop application starts and renders
on WebView2, the readiness screen runs, and the application's own Go layer
drives `debark` here exactly as it does on Linux. All of that was done on a
real Windows 11 machine and is recorded below with the commands that did it.

**None of it is a support commitment.** `.github/workflows/ci.yml` already says
why the Windows job exists — *"Windows is built and tested because it compiles
and people develop on it, not because it is supported"* — and this document is
the operator-facing form of that sentence. Nothing here says Debark works on
your Windows machine, and two of the things below say plainly that it will not
work on some of them.

The shipping target is Linux. A bundle is an ordinary folder, so building it on
a Linux machine you can reach and copying it back is a first-class way to work
and is what the readiness screen itself offers when the local routes are shut.

---

## 1. The machine, and where the binaries came from

| | |
|---|---|
| **Host** | Windows 11 Pro, build 26200, `windows/amd64`, Intel Core Ultra 9 285K (24 logical) |
| **Go** | 1.26.0 `windows/amd64` |
| **Docker** | 29.6.2, client and server, Linux containers, WSL 2 backend (`docker-desktop` appears in `wsl -l -v`) |
| **WSL** | WSL version 2.6.3.0, kernel 6.6.87.2-1. Three registered distributions, all version 2: `Ubuntu` (stopped, default), `docker-desktop` (running), `tt-runner` (stopped) |
| **Webview** | WebView2. Wails v2.12.0, `go-webview2` v1.0.22 |
| **debark-gui** | `9770e74`, clean except for one untracked file belonging to another package |
| **debark** | built from the engine at `6549f6f`. **That tree had four files modified that are not this round's work** — `README.md`, `internal/cli/cmd_build.go`, `internal/cli/cmd_frombase.go`, `internal/cli/root.go`, all help-text prose from unrelated work. They were neither staged nor reverted. The binaries below therefore came from `6549f6f` plus help-text edits, and nothing else. |
| **When** | 2026-09-06 |

Two `debark` binaries and two harnesses were built, all outside both
repositories, into the scratch directory:

```
# in ..
go build -trimpath -o <scratch>/runtime/debark.exe ./cmd/debark
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -o <scratch>/runtime/bin/debark-linux-amd64 ./cmd/debark

# in debark-gui
go build -trimpath -o <scratch>/gui-e2e.exe ./hack/e2e
go build -tags "desktop,production" -ldflags "-H windowsgui" -trimpath \
    -o <scratch>/debark-gui.exe .
```

`debark.exe version` reports `debark dev (community)`, commit
`6549f6feefa9574ac9ee307bcec11d2636f58cc8`, `go1.26.0`, `platform:
windows/amd64`.

Everything in §3 and §4 was driven through **the application's own Go layer** —
`hack/e2e`, which calls `internal/app` on top of `internal/cliadapter` through
the same bound methods the frontend calls. Nothing below shells out to
`debark` directly, because that would prove `debark` works, which was never
in doubt. §2 was driven through the shipping desktop binary itself.

---

## 2. The desktop application starts and renders on WebView2

The real `debark-gui.exe`, built with `desktop,production`, was started twice
from PowerShell with `Start-Process -PassThru`, photographed with `PrintWindow`
against its own window handle — so nothing else on the desktop was captured —
and stopped with `Stop-Process -Id <the pid we started>`. Never by name.

| | |
|---|---|
| Window title | `Debark` |
| Window size as measured | 1053 × 820, and 1053 × 1011 on the second run |
| Readiness report completes in | 770 ms and 1036 ms |
| Screenshots | `readiness-windows.png`, `readiness-windows-bare.png` in the scratch directory |

The screen renders correctly: the stepper headerbar, the "Before you start"
heading, the headline banner and the check table all appear as designed. This
is the first time the application has been photographed on WebView2; every
earlier harness in this repository is Chromium, and the shipping engine on
Linux is WebKit2GTK.

**Two things could not be driven programmatically** and are recorded as
unknowns rather than passes:

- `SetWindowPos` did not enlarge the window past ~1053 px, so no capture shows
  the check rows further down the page. Their contents are recorded in §5 from
  the backend's own JSON instead, which is the authoritative source for them.
- `SendKeys` into the window fails with `Access is denied` — the automating
  process and the application are at different integrity levels — so nothing
  was scrolled, clicked or typed. **No keyboard or interaction testing was done
  on Windows at all.**

---

## 3. The container backend now works from Windows, up to a point

This is what changed this round. `.github/workflows/ci.yml`'s `e2e` comment
records, as a measured fact, that `debark build` **could not use the container
backend from a Windows host at all**: it mounted its own executable into the
container and refused the Windows PE binary, and no flag, config key or
environment variable pointed it anywhere else. Core `a0b61c2` added
`--self-binary`, `DEBARK_SELF_BINARY`, a `self_binary` config key and
discovery of `bin/debark-linux-<arch>` beside the binary. Both halves were
re-tested here.

### 3.1 Without a Linux debark — the failure is now explained

`debark.exe` alone in a directory, nothing else set:

```
<scratch>/gui-e2e.exe -debark <scratch>/runtime-bare/debark.exe \
    -work <scratch>/work-bare -base debian:12/minimal -packages jq
```

`AppInfo`, `StartReadinessCheck`, `keygen`, `ListBases`, `SelectTarget`,
`AddPackages` and `PreviewCommand` all succeeded. `StartBuild` failed:

```
[cli.failed] this build has to run apt inside a Linux container, and the only
debark it can find to put in that container is …\runtime-bare\debark.exe,
which is not a Linux program
hint: Put a Linux build of debark named bin/debark-linux-<arch> beside the
debark this application runs — that path is found with no further setting —
or run the build on Linux, or on WSL. Nothing is wrong with the target you
picked; this machine cannot reach it from here.
```

That is `docs/dev/error-catalogue.md` §3.9, re-caused on the machine it was
captured on, and it is the improvement `a0b61c2` bought: the underlying
`debark` message is still the ELF magic-number dump, but the operator no
longer sees it.

### 3.2 With `bin/debark-linux-amd64` beside the binary — apt ran in a container

The same command against `<scratch>/runtime/debark.exe`, with the static
`linux/amd64` build sitting at `runtime/bin/debark-linux-amd64` and **no flag,
no environment variable and no config file**:

- the `self-binary` readiness row turned green;
- `StartBuild` ran for **64.9 s**, emitting `build:started`, 6 × `build:progress`,
  8 × `build:event` and `build:finished`;
- the NDJSON stream carried 7 × `backend.selected` and 1 × `snapshot.loaded`;
- and the failure it eventually reported quotes **apt's own answer from inside
  a Debian 12 container**: `plan says 1.6-2.1+deb12u2
  (jq_1.6-2.1+deb12u2_amd64.deb)`.

So the sibling discovery works, the Linux binary was mounted and re-executed,
and apt resolved inside a Linux container **driven by the GUI's Go layer running
natively on Windows**. That path did not exist before this round.

### 3.3 Where the mounted Linux binary comes from — and the answer an operator gets

Of the four mechanisms, **the GUI uses exactly one, and it uses it by not doing
anything**:

| Mechanism | Does the GUI use it? |
|---|---|
| `--self-binary` flag | **No.** `internal/cliadapter` never emits the flag; `BuildSpec` has no field for it. |
| `DEBARK_SELF_BINARY` | Only by inheritance. The GUI sets nothing; if the operator has exported it, the child `debark` sees it. `internal/readiness` reads it for its check. |
| `self_binary` config key | Only because `debark` reads its own config. The GUI never writes one, and the readiness check explicitly does not read it. |
| `bin/debark-linux-<arch>` beside the `debark` binary | **Yes — this is the only route the GUI actually exercises**, and it is what §3.2 used. |

**So the honest answer to "where does the mounted Linux binary come from" is:
somebody has to put it there, and today the only person who can is somebody with
a Go toolchain and a checkout of the `debark` source.** The readiness row's
own remedy is a cross-compile command:

```
env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/debark-linux-amd64 ./cmd/debark
```

with the note *"Run this in a checkout of the debark source, then copy the
result to `bin/debark-linux-amd64` beside the debark you are running."*

**An operator who installed Debark and `debark` from packages would not have
one.** Nothing in this repository ships a `linux/amd64` `debark`, nothing
downloads one — rule 3 forbids a debark-operated endpoint, and this would be
one — and no Windows installer places one. The row is honest about what is
missing and gives a correct command; it is a correct command that a Windows
operator without Go cannot run. That gap is a packaging decision, not a bug, and
it belongs to whoever owns the Windows artefact if one is ever produced.

---

## 4. The end-to-end run did **not** complete, and why

The definition-of-done item is *"a GUI-built bundle verifies and installs on an
offline target, `--network none`"*. On Linux `hack/demo-gui-e2e.sh` proves it.
**On Windows it was not proved, and the reason is not the container backend.**

Every container build from a Windows host writes its `/archives` into a shared,
well-known Docker **named volume**, `debark-archives-cache`
(`core/apt/container.go`, `containerDefaultStoreVolume`). On this machine that
volume already existed, left by earlier work, holding seven `.deb` files from
two different targets:

```
$ docker run --rm -v debark-archives-cache:/a:ro debian:bookworm-slim ls -la /a
hello_2.10-3_amd64.deb
jq_1.6-2.1+deb12u2_amd64.deb                 libjq1_1.6-2.1+deb12u2_amd64.deb
jq_1.7.1-3ubuntu0.24.04.2_amd64.deb          libjq1_1.7.1-3ubuntu0.24.04.2_amd64.deb
libonig5_6.9.8-1_amd64.deb                   libonig5_6.9.9-1build1_amd64.deb
partial/
```

`scanArchivesDir` (`internal/cli/cmd_resolve.go`) puts **every** `.deb` in that
directory into the resolve envelope's `files[]`, keyed by name and architecture,
so the newer filename wins. `containerCrossCheckEnvelope`
(`core/apt/container_driver.go`) then requires that files[] and the plan agree
exactly, in both directions. The build failed at `exit 4`, class `verification`:

```
[cli.failed] container: envelope plan/files disagree for jq:
plan says 1.6-2.1+deb12u2 (jq_1.6-2.1+deb12u2_amd64.deb),
files says 1.7.1-3ubuntu0.24.04.2 (jq_1.7.1-3ubuntu0.24.04.2_amd64.deb)
hint: Do not install a bundle that fails verification. Obtain a fresh copy from
the machine that built it, and check that the key you trust is the one it was
signed with.
```

Three things about that, each measured rather than reasoned:

1. **It is not confined to a "second build with a different package set".** The
   cross-check rejects any file in `files[]` the plan did not select, as well as
   any disagreement about one it did. With those seven files present, *every*
   container build from this host fails — any target, any architecture, any
   package list — before apt's work is ever used.
2. **There is no operator-reachable way out.** `ContainerOptions.StoreVolume` is
   never set by anything outside a unit test; `debark build --help` has no
   flag for it; the full config surface
   (`internal/cli/config/config.go`) has `store_dir` and no `store_volume`; and
   no `DEBARK_*` variable names it. `debark store gc` operates on the
   content-addressed object store, not on this volume. The only remedy is
   `docker volume rm debark-archives-cache`, which appears in no message, no
   help text and no document the operator will read.
3. **The message the GUI shows is a tampering message for a stale cache.** The
   hint tells an operator that their bundle may have been altered in transit and
   to obtain a fresh copy — for a build that never produced a bundle, on a
   machine where nothing is wrong except a cache directory.

Clearing that volume needs a decision about shared state on a machine running
other work, and the permission to do it was not available to this package. **So
the Windows end-to-end run stops here: bundle not built, therefore not verified,
therefore not installed offline.** Nothing was worked around; the volume was
read and left exactly as it was found.

**Untested on Windows, in consequence:** `StartVerify`, `PlanExport`,
`StartExport`, the `--network none` install, and running the installed software.
All of those are proved on Linux and none of them are proved here.

---

## 5. What the readiness screen claims on Windows, measured

> **Superseded, and kept because it is the evidence.** Everything in §5 and §6
> was measured on 2026-09-07 and was true then. The defect it records has since
> been fixed: `cbaec92` stops `DeriveBuildEnvironment` counting `CheckWSL` as a
> build route, and rewrites the `wsl` row's own summary — which was the other
> half of the defect, since it said *"WSL 2 is available, so builds can run
> without a container"* in prose. A follow-up corrected `bindRemoteBuilder`'s
> counters, which were independently wrong in a way the derivation fix did not
> reach: they required *every* local route blocked, but Windows has one local
> route that takes two rows to work, so a machine with healthy WSL 2 and a
> broken container route never saw the remote-builder panel at all.
>
> **So do not read the table below as current behaviour.** It is the record of
> what a real machine was told, and it is why the fix exists. The numbers were
> honest when taken; that is the only reason to keep them rather than edit them
> into agreement with the code.

Both runs in §3 wrote the full `ReadinessReport`. This is what it said with no
Linux `debark` present — the state a fresh Windows install is in:

| Row | Status | Severity | Summary |
|---|---|---|---|
| `debark-binary` | ok | info | debark dev is installed and answering. |
| `wsl` | **ok** | info | WSL 2.6.3.0 is available, **so builds can run without a container.** |
| `container` | problem | degraded | Docker is installed but did not answer within the timeout. |
| `self-binary` | problem | degraded | No Linux debark was found … container builds will fail on this Windows host even though a container runtime is present. |
| `build-environment` | **ok** | info | **This machine can run a build using WSL 2.** |
| `signing-key` | problem | info | No operator signing key was found. |
| `disk-space` | ok | info | 1.7 TiB free. |

`can_build: true`, `blocking_count: 0`. And then **every build failed.**

Rendered, and photographed in the shipping application on WebView2, that is a
green banner reading **"This machine can build bundles."** above the words
*"None of them stop you, and every one can be dealt with later without starting
over."*, with a footer saying *"Nothing here stops you."* and a primary
**Continue** button.

Every place this over-promises, in the order a person meets them:

1. **The `wsl` row promises a build route that does not exist.** *"WSL 2 …
   is available, so builds can run without a container"* is false in this
   product: nothing in `internal/cliadapter` ever runs `wsl.exe`, and
   `--backend` takes only `auto`, `local` or `container`.
2. **The derived `build-environment` row inherits it** — *"This machine can run
   a build using WSL 2"* — and it is the row that decides `can_build`.
3. **`can_build: true` is therefore wrong on this machine**, and it governs the
   build button.
4. **The headline says the opposite of what will happen.** "This machine can
   build bundles" / "None of them stop you", green, on a machine where no build
   can succeed.
5. **The footer repeats it** — "Nothing here stops you" — and the Continue
   button is styled primary rather than "Continue anyway".
6. **The self-binary row is the only honest line on the screen**, and it is
   labelled *Limited*, one step below the row that contradicts it.
7. **The remote-builder panel never appears**, so the one route that would
   actually work on this machine is not offered. `bindRemoteBuilder`
   (`internal/app/bindings.go`) counts only `wsl` and `container` as local
   options; `self-binary` is not in that set, so `localBlocked == local` is
   false while the WSL row is green.
8. **The `container` row under-reports a working Docker.** Measured on this
   host: the first `docker info` after an idle period took **6431 ms**, and the
   next two took 558 ms and 455 ms. The check's per-command budget is five
   seconds, so a perfectly healthy Docker Desktop reports *"installed but did
   not answer within the timeout"* on a cold first run and passes on the next —
   which is exactly what happened between the two runs in §3.

Points 1–7 are one defect with one cause, set out in §6. Point 8 is separate
and is a false negative rather than an over-promise.

---

## 6. The WSL 2 defect, stated so it can be acted on

> **Acted on, and fixed — see the note at the head of §5.** This section did its
> job: it was written to be handed to whoever owned `internal/readiness`, and
> the truth table below is what settled the decision. It is kept as the evidence
> for a change that has since landed, not as a description of current code.

`DeriveBuildEnvironment` (`internal/readiness/readiness.go`) counts three rows
as build routes — `apt`, `container` and **`wsl`** — and concludes `ok` if any
one of them is `ok`. `core/lock` knows only `local`, `container` and `auto`, and
nothing in this repository invokes `wsl.exe`. So the WSL row contributes a route
that cannot be taken.

The consequence is sharper than "a phantom route", and it is the part worth
carrying into the decision:

> **The WSL row silently defeats the `self-binary` gate.**

That gate was added this round precisely so a Windows machine with Docker and
no Linux `debark` would stop claiming it could build. It is implemented as the
`containerOnlyBlocked` branch of `DeriveBuildEnvironment` — and that branch is
only reachable when **no other route is `ok`**, which on Windows means the WSL
row must also be failing. Driving the derivation with fabricated rows:

| `wsl` | `container` | `self-binary` | → `build-environment` | `can_build` |
|---|---|---|---|---|
| ok | ok | **problem** | ok — "can run a build using WSL 2" | **true** |
| ok | problem | **problem** | ok — "can run a build using WSL 2" | **true** |
| problem | ok | **problem** | problem, blocking — the gate fires | false |
| ok | ok | ok | ok | true |

Row 3 is the only one where the gate works, and it needs a Windows machine with
**no** WSL 2. **Docker Desktop's own default backend on Windows is WSL 2** — on
this host `docker-desktop` is itself a registered WSL 2 distribution — so on the
machines the gate was built for, the WSL row is green and the gate can
essentially never fire. Row 1 is this machine, and §3.1 is that row measured end
to end: `can_build: true`, and the build failed.

The observation was made read-only. Docker was not stopped to create the failing
state, no WSL distribution was started, stopped, converted or unregistered, and
nothing was written to `internal/readiness`. The table above came from a test
file injected with `go test -overlay` from the scratch directory.

**This is a product decision, not a bug to code around**, and it is not this
document's to make: changing the derivation contradicts
`docs/dev/binding-surface.md`, which is frozen, so the derivation and the
document have to change together. The evidence is here so that decision does not
have to be re-derived.

---

## 7. What is true, what is false, and what is unknown

**True on Windows, measured:**

- `debark-gui` compiles and `go vet` is clean for `windows/amd64`.
- The desktop application starts on WebView2 and renders the readiness screen.
- The readiness checks run: `debark` discovery, WSL enumeration, Docker,
  self-binary, signing key, disk space.
- `AppInfo`, `StartReadinessCheck`, `RunReadinessAction`, `SupportedArchitectures`,
  `ListBases`, `SelectTarget`, `AddPackages` and `PreviewCommand` all work
  through the real bound methods.
- `keygen` runs and writes a usable operator key.
- The container backend is reachable: with `bin/debark-linux-amd64` beside the
  binary and no flag at all, apt resolves inside a Debian 12 container.

**False on Windows, measured:**

- The readiness screen's claim that this machine can build (§5, §6).
- Any container build on a host whose `debark-archives-cache` volume holds
  anything the current plan does not select (§4).

**Unknown on Windows — not tested, not claimed:**

- A complete build producing a bundle.
- `StartVerify`, `PlanExport`, `StartExport`.
- The `--network none` offline install and running the installed software.
- Every keyboard interaction, every other screen, and the catalogue, picker,
  build and export screens as rendered by WebView2.
- Any screen reader. See `docs/accessibility.md` §1.
- `wails build` as a packaging step, and `build/windows/installer/`.
- Any Windows version other than 11 Pro build 26200, and any Docker
  configuration other than Docker Desktop 29.6.2 in Linux-container mode.

---

## 8. Reproducing all of it

```
# 1. binaries, all outside the repository
cd ..
go build -trimpath -o <scratch>/runtime/debark.exe ./cmd/debark
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -o <scratch>/runtime/bin/debark-linux-amd64 ./cmd/debark
cd gui
go build -trimpath -o <scratch>/gui-e2e.exe ./hack/e2e
go build -tags "desktop,production" -ldflags "-H windowsgui" -trimpath \
    -o <scratch>/debark-gui.exe .

# 2. the §3.1 failure: debark.exe on its own
cp <scratch>/runtime/debark.exe <scratch>/runtime-bare/
<scratch>/gui-e2e.exe -debark <scratch>/runtime-bare/debark.exe \
    -work <scratch>/work-bare -base debian:12/minimal -packages jq

# 3. the §3.2 result: the sibling is found with no flag
<scratch>/gui-e2e.exe -debark <scratch>/runtime/debark.exe \
    -work <scratch>/work-win -base debian:12/minimal -packages jq

# 4. the state that stops it (read-only)
docker run --rm -v debark-archives-cache:/a:ro debian:bookworm-slim ls -la /a
```

Both runs write a full JSON report at `<work>/gui-e2e-report.json`, including
the whole `ReadinessReport`, the event counts and the `UIError`.

The desktop binary is started and stopped from PowerShell by the PID
`Start-Process -PassThru` returns, and photographed with `PrintWindow` against
its own window handle. **Never stop a process by name on a machine somebody is
using**, and do not screenshot the desktop when the window's own buffer will do.
