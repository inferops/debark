# Packaging — the Debark `.deb`, and the Windows installer that is not built

The record for the Debian packaging work. What the package is, how it is built,
how it is proved, and the two open questions that work was asked to settle.

Everything below was measured. Where a number or a claim came from a command,
the command is here.

---

## 1. The package

| | |
|---|---|
| Package name | `debark-gui` |
| App id (AppStream component) | `io.github.inferops.Debark` |
| Version scheme | the upstream version with **no Debian revision** — a native package, matching the CLI's `.goreleaser.yaml`, which packages `{{ .Version }}` with no revision either. An untagged build gets `0.0.0~git<commit-count>.g<sha>`; the `~` sorts it below every real release, so a developer build can never shadow a released one. A tag that *does* carry a hyphen is a case this scheme did not cover, and §9 records what happens and what was done about it. |
| Architecture | derived from `dpkg --print-architecture` at build time |
| Section / Priority | `admin` / `optional` |
| Compression | `xz -9` (`dpkg-deb -Zxz -z9`) |
| `.deb` size | 4,522,428 bytes (4.31 MiB) |
| Installed size | 15,227 KiB — 15,541,504 bytes of that is the binary |
| Maintainer scripts | **none.** `DEBIAN/` holds `control` and `md5sums` and nothing else |

Every measurement in this document was **re-taken against a release-path
package**: `debark-gui_1.2.3_amd64.deb`, built by `packaging/build-deb.sh`
from a binary this repository produced at commit **`aeb80d0`** with exactly
`.goreleaser.yaml`'s flags —

```
CGO_ENABLED=1 go build -tags "desktop,production,webkit2_41" \
    -trimpath -buildmode=pie -ldflags "-s -w -X main.version=1.2.3" .
```

**That matters, and it is why the sizes above moved.** The first revision of
this document measured a package built with `-tags … -trimpath` and nothing
else — no `-s -w`, no `-buildmode=pie` — because that was the command in
`packaging/build-deb.sh`'s header, and the header did not match the release.
That binary is 20,899,008 bytes; the shipped one is 15,541,504. A document
describing a package the release never produces is the same defect as a
checklist run against one, and §9 records the `lintian` error that came out of
it.

### `Depends`

```
libc6 (>= 2.34), libgdk-pixbuf-2.0-0 (>= 2.22.0), libglib2.0-0t64 (>= 2.16.0),
libgtk-3-0t64 (>= 3.21.4), libjavascriptcoregtk-4.1-0, libsoup-3.0-0 (>= 2.4.0),
libwebkit2gtk-4.1-0 (>= 2.39.91)
```

No `Recommends`, no `Suggests`, no `Pre-Depends`, no `Conflicts`.

**These are derived, not written.** `packaging/build-deb.sh` runs
`dpkg-shlibdeps -O --ignore-missing-info` against the real ELF, which reads
its `DT_NEEDED` entries, maps each SONAME to the package owning it, and takes
the minimum version from the *symbols this build actually references*. There
is no hand-written dependency list anywhere in this repository, deliberately:
the GUI links GTK through cgo and **cannot be `CGO_ENABLED=0` on Linux**, so
its shared-library surface is a property of the build rather than of a
document, and it changes when the code does. A hand-written list would be
correct until the first time somebody imported something.

What being a cgo binary means for the package, concretely:

- It has a real `DT_NEEDED` list, so `dpkg-shlibdeps` works and the package
  declares honest library dependencies. A `CGO_ENABLED=0` Go binary declares
  nothing and depends on nothing, which is simpler and is not available here.
- It is architecture-specific and cannot be `Architecture: all`.
- It is bound to the glibc ABI (`libc6 (>= 2.34)`) and, through the t64 names
  below, to a fairly recent distribution baseline.
- Reproducibility for the release work means something narrower than for the
  CLI; see §6.

### What is in it

```
/usr/bin/debark-gui                                          0755
/usr/share/applications/debark-gui.desktop                   0644
/usr/share/icons/hicolor/{16,24,32,48,64,128,256,512}/apps/debark-gui.png
/usr/share/metainfo/io.github.inferops.Debark.metainfo.xml   0644
/usr/share/doc/debark-gui/copyright                          0644
/usr/share/doc/debark-gui/changelog.gz                       0644
/usr/share/man/man1/debark-gui.1.gz                          0644
```

Every entry is `root:root`; directories 0755, data 0644, the binary 0755.
Nothing is installed outside the six prefixes `docs/security-review.md` §5
row 12 permits — `/usr/share/man` is the sixth, and the man page is why it is
there.

`changelog.gz` is the **native** package's name for that file. A version
carrying a hyphen is not native and its changelog must be
`changelog.Debian.gz` instead; `build-deb.sh` chooses between the two. §9 has
the reproduction that made that necessary.

There is no `scalable/apps/*.svg`. The only icon source in the tree is
`build/appicon.png`, a 1024×1024 raster; the eight hicolor sizes are
downscaled from it with ImageMagick. If a vector icon is ever drawn, drop it
at `packaging/icons/hicolor/scalable/apps/debark-gui.svg` and
`build-deb.sh` picks it up with no change — it globs `*.png` and `*.svg`
under `packaging/icons/`.

### Why there are no maintainer scripts at all

A package that installs a `.desktop` file and icons normally carries a
`postinst` running `update-desktop-database` and `gtk-update-icon-cache`.
This one does not, because it does not need to: `desktop-file-utils` owns a
dpkg trigger on `/usr/share/applications` and `hicolor-icon-theme` owns one
on `/usr/share/icons/hicolor`, and both fire on install and on removal. That
is visible in the row-19 transcript:

```
Setting up debark-gui (0.1.0) ...
Processing triggers for desktop-file-utils (0.27-2build1) ...
Processing triggers for hicolor-icon-theme (0.17-2) ...
```

So the caches are updated and **nothing in this package runs as root at
install time**. `docs/security-review.md` §5 row 3 asks for exactly this and
calls it "if that is achievable". It is achievable, and rows 4 and 5 are then
vacuous rather than argued.

`hicolor-icon-theme` is not in `Depends`. It does not need to be: `libgtk-3-0t64`
depends on `libgtk-3-common`, which depends on `hicolor-icon-theme`, so it is
always present wherever this package installs. Leaving it out keeps §5 row 14
("libraries only") literally true rather than nearly true.

`debark` itself is not in `Depends` or `Suggests` either. It is not in any
distribution archive, so depending on it would make the package
uninstallable; and a missing `debark` is precisely what the readiness screen
exists to explain. That is the shape §5.2 asks for — a documented condition
the operator resolves, not a maintainer script that resolves it for them.

---

## 2. Which releases this installs on, and which it does not

Measured by attempting the install, not by reading a compatibility table.

| Release | WebKitGTK 4.1 in the archive | GTK/GLib package names | This `.deb` installs? |
|---|---|---|---|
| Ubuntu 22.04 LTS (jammy) | **yes** — 2.36.0-2ubuntu1 at GA (universe), 2.50.4 in `-updates` | `libgtk-3-0`, `libglib2.0-0` | **no** |
| Ubuntu 24.04 LTS (noble) | 2.44.0-2 at GA, 2.52.6 in `-updates` | `libgtk-3-0t64`, `libglib2.0-0t64` | **yes** |
| Ubuntu 25.10 (questing) | 2.48.6-1ubuntu2 at GA, 2.52.3 in `-updates` | t64 | yes (not exercised) |
| Ubuntu 26.04 LTS (resolute) | 2.52.0-1 at GA, 2.52.6 in `-updates` | t64 | **yes** |
| Debian 12 (bookworm) | yes — 2.50.6-1~deb12u2 | `libgtk-3-0`, `libglib2.0-0` | **no** |
| Debian 13 (trixie) | 2.52.3-2~deb13u1, 2.52.6 in security | t64 | **yes** |

**The Makefile's `WEBKIT_TAG` comment is wrong about 22.04**, and it matters
because it is the only place this project writes the compatibility rule down:

> `# no tag means webkit2gtk-4.0, `webkit2_41` means webkit2gtk-4.1. Debian 13,`
> `# Ubuntu 24.04 and everything newer ship only 4.1, so 4.1 is the default here`
> `# and 4.0 is the exception:`
> `#     make build WEBKIT_TAG=          # webkit2gtk-4.0 (Ubuntu 22.04, Debian 12)`

Ubuntu 22.04 ships **both** ABIs — `libwebkit2gtk-4.1-0` is in
`jammy/universe` at 2.36.0-2ubuntu1 and in `jammy-updates/universe` at
2.50.4-0ubuntu0.22.04.1. So does Debian 12. Building with `WEBKIT_TAG=` for
those releases is not required by the WebKit ABI. Reported to the release work,
which owns the `Makefile`.

**What actually excludes 22.04 and Debian 12 is the `t64` transition, not
WebKit.** Both releases predate the 64-bit `time_t` ABI break, so their GTK
and GLib packages are named `libgtk-3-0` and `libglib2.0-0`, and
`dpkg-shlibdeps` on a noble build necessarily emits the `t64` names. The
failure is clean and readable:

```
$ docker run --rm -v .../deb:/deb:ro ubuntu:22.04 \
    bash -c 'apt-get update -qq; apt-get install -y --no-install-recommends /deb/*.deb'
The following packages have unmet dependencies:
 debark-gui : Depends: libglib2.0-0t64 (>= 2.16.0) but it is not installable
                Depends: libgtk-3-0t64 (>= 3.21.4) but it is not installable
E: Unable to correct problems, you have held broken packages.
```

Identical output on `debian:12`.

Supporting 22.04 or Debian 12 would mean a second build on a pre-t64 base
image producing a second `.deb` with `libgtk-3-0`/`libglib2.0-0` in its
`Depends`. That is a release-pipeline decision, not a packaging-script one:
`build-deb.sh` already produces the right answer on whatever base it is run
on, because the answer is derived. Nothing here needs to change for it.

**Supported set, as shipped: Ubuntu 24.04 LTS and newer, and Debian 13 and
newer.**

---

## 3. The `light-dark()` question, settled

`frontend/src/design/README.md` §1 and §10.2 avoid `light-dark()`, CSS
nesting, `@layer` and `:has()` because "all four are used below the version of
WebKitGTK that Debian stable and older Ubuntu LTS ship", and §10.2 says:
"**Verify the actual minimum WebKitGTK version the `.deb` will declare**; if
it is recent, revisit this."

### What the package declares, and what that is worth

`dpkg-shlibdeps` derives `libwebkit2gtk-4.1-0 (>= 2.39.91)`. **That number is
not the answer**, and reading it as one would be a mistake. It is a
*symbol-availability* floor: the oldest `libwebkit2gtk-4.1-0` whose exported
symbols cover the ones this binary references. It says nothing about CSS.

The number that matters is the oldest WebKitGTK that can be running on a
machine where this package installs at all. From the table in §2, that is
**Ubuntu 24.04 at GA, never updated: WebKitGTK 2.44.0-2**. Every other
installable release starts higher (25.10 at 2.48.6, 26.04 at 2.52.0,
Debian 13 at 2.52.3).

### The measurement

Not caniuse, not a version table: the real engine, through its own GObject
bindings, in a container with `libwebkit2gtk-4.1-0` downgraded to `2.44.0-2`
from `noble/main` while every other library stayed at the point-release level.

The probe loads a stylesheet using all four features and reads back
`getComputedStyle(...).color`, so a feature that merely *parses* without
*resolving* is caught:

```css
:root { color-scheme: light; }
#ld  { color: light-dark(rgb(1, 2, 3), rgb(4, 5, 6)); }
.nest-outer { & .nest-inner { color: rgb(7, 8, 9); } }
@layer probe { #lay { color: rgb(10, 11, 12); } }
.has-parent:has(> .has-child) { color: rgb(13, 14, 15); }
```

| Engine | release it belongs to | `light-dark()` | nesting | `@layer` | `:has()` |
|---|---|---|---|---|---|
| **2.36.0-2ubuntu1** | Ubuntu 22.04 at GA | **no** — `rgb(0, 0, 0)`, `CSS.supports` false | **no** — `rgb(0, 0, 0)` | yes | yes |
| **2.44.0-2** | **Ubuntu 24.04 at GA — the package's floor** | **yes** — `rgb(1, 2, 3)` | **yes** — `rgb(7, 8, 9)` | yes | yes |
| 2.52.6-0ubuntu0.24.04.1 | Ubuntu 24.04 today | yes | yes | yes | yes |
| 2.52.6-1~deb13u1 | Debian 13 today | yes | yes | yes | yes |

The 2.36.0 row is the control, and it earns its place: it shows the caution
was **right when it was written** and that the probe can detect the failure it
was written about. `light-dark()` and nesting genuinely do not work on a
22.04-era engine. `@layer` and `:has()` worked even there, which means two of
the four exclusions were never needed on any engine this project could have
targeted.

The user-agent strings, for the record: 2.36.0 reports `Version/15.0`,
2.44.0 `Version/17.0`, 2.52.6 `Version/60.5`.

### The recommendation

**Yes — drop the duplicated dark block and use `light-dark()`.** The
condition §10.2 set ("if the project decides to depend on WebKitGTK 2.44+") is
already met and is not a new decision: the package cannot install anywhere
older, and the `t64` dependency names enforce that mechanically rather than by
convention. Nothing needs to be added to `Depends` to make it true.

**The saving.** `frontend/src/design/README.md` §3 records **62 custom
properties plus `color-scheme`** duplicated across `DARK BLOCK 1 of 2` and
`DARK BLOCK 2 of 2` in `tokens.css` (§10.2 calls it 54 declarations; §3's 62
is the later count). One of the two blocks goes, and with it the rule in §3
that says "if you edit one dark block you must edit the other", and the
assertion in the §7.3 audit script that exists only to enforce that rule. The
README's own words: the duplication "is only safe because something checks
it" — `light-dark()` removes the thing that has to be checked.

**Two cautions for whoever does it**, neither of which changes the answer:

1. `light-dark()` resolves against the element's used `color-scheme`, so
   every root that carries a palette must also carry `color-scheme`. The
   design system already sets `color-scheme` alongside each palette (§3), so
   this is a property the tree has rather than one it must acquire.
2. The `data-theme="light"` / `data-theme="dark"` override still needs a rule
   — `light-dark()` follows `color-scheme`, so the attribute selectors set
   `color-scheme: light` / `dark` and the colour tokens follow. That is one
   pair of two-line rules replacing a 62-declaration block, not a wash.

**I did not change any CSS.** `frontend/src/design/` belongs to the
accessibility follow-up. This is a finding and a recommendation, for whoever
owns that file to route.

### Reproducing the measurement

```bash
# WebKitGTK pinned to Ubuntu 24.04's GA version, everything else current.
docker build -t debark-css:webkit2.44 --build-arg WEBKIT=2.44.0-2 - <<'EOF'
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      python3-gi gir1.2-gtk-3.0 xvfb && rm -rf /var/lib/apt/lists/*
ARG WEBKIT
RUN apt-get update && apt-get install -y --no-install-recommends --allow-downgrades \
      libwebkit2gtk-4.1-0=${WEBKIT} libjavascriptcoregtk-4.1-0=${WEBKIT} \
      gir1.2-webkit2-4.1=${WEBKIT} gir1.2-javascriptcoregtk-4.1=${WEBKIT}
EOF
# then, inside, under Xvfb with WEBKIT_DISABLE_COMPOSITING_MODE=1:
#   a ~30-line PyGObject script that loads the stylesheet above into a
#   WebKit2.WebView, reads getComputedStyle().color for each element and
#   prints them as JSON.
```

The probe script is not committed. It is thirty lines, it duplicates nothing
in the tree, and a second copy of a CSS feature list is a second thing to keep
in step — the same reason `frontend/src/design/README.md` §7.3 gives for not
committing the contrast audit.

---

## 4. The acceptance test — `docs/security-review.md` §5

All twenty rows are implemented in `packaging/verify-deb.sh`, which prints
each row's command and its actual output rather than a tick. Run it inside a
throwaway container:

```
docker run --rm -v <repo>:/repo:ro -v <out>:/out debark-deb-test:noble \
    bash /repo/packaging/verify-deb.sh --display :101 /out/deb/debark-gui_*.deb
```

`debark-deb-test:<release>` is a **stock** distribution image plus the tools
rows 2, 18 and 20 need, plus every runtime dependency of the package —
obtained by installing the `.deb` with apt during the image build and then
`dpkg -r`-ing it. That last step is what makes row 19 mean anything: if the
GTK and WebKitGTK stacks arrived during the measured install, the diff would
be dominated by dbus, at-spi and fontconfig doing legitimate things and the
question the row asks would be buried. `sudo` and `policykit-1` are never
installed, because row 20 requires their absence.

**Result: 20 of 20 pass on all three supported releases, against the
release-path package — the stripped, PIE one `.goreleaser.yaml` actually
produces. Nothing was skipped and nothing was waived.**

| Release the run was on | image | result |
|---|---|---|
| Ubuntu 24.04.4 LTS (noble) | `ubuntu:24.04` | 20 passed, 0 failed, 0 skipped |
| Ubuntu 26.04 LTS (resolute) | `ubuntu:26.04` | 20 passed, 0 failed, 0 skipped |
| Debian 13 (trixie) | `debian:13` | 20 passed, 0 failed, 0 skipped |

**"Against the release-path package" is the load-bearing clause and it was not
always true.** The first run of this checklist was against a package built with
the command in `build-deb.sh`'s header, which omitted `-s -w` and
`-buildmode=pie`. When the release work hardened the link, the shipped package
started scoring **19/20**: row 16 corroborated the nine glibc setuid-family
imports against the `_cgo_libc_set*` wrappers in the *static* symbol table, and
`-s` removes it — nine wrappers unstripped, zero stripped
(`docs/release.md` §4.7). The property row 16 asks about held; the evidence
path did not exist on the artefact that ships. Row 16 has been reworked so its
corroboration survives stripping, and the three runs above are the result. See
"Row 16" below, including the three negative controls that make the reworked
row fail.

Transcripts and the three row-20 photographs were not committed to this
repository; the run is reproducible from the command above.

### `lintian` on the package that ships

§5 row 18 words it as `lintian $DEB`, and that plain form is what a
distribution maintainer or a suspicious operator will run. Against the
release-path package it is empty:

```
$ lintian /out/deb-release/debark-gui_1.2.3_amd64.deb
running with root privileges is not recommended!
$ echo $?
0
```

The one line is `lintian`'s own advisory about being run as root inside the
container; it is not a tag. No `E`, no `W`, no `I`, no `P`.

`verify-deb.sh` runs the louder form, and that is not empty — but everything in
it is informational:

```
$ lintian --no-cfg --display-info --display-experimental --pedantic $DEB
I: debark-gui: binary-has-unneeded-section .comment [usr/bin/debark-gui]
I: debark-gui: spelling-error-in-binary classs class [usr/bin/debark-gui]
I: debark-gui: spelling-error-in-binary containe contained [usr/bin/debark-gui]
$ echo $?
0
```

The two spelling hits are inside vendored strings compiled into the binary and
are not this project's text.

### Every tag is attributable to a flag

Four binaries from one source, four packages through
`packaging/build-deb.sh`, one `lintian` each. This is `docs/release.md`
§4.7's table, re-measured here rather than cited:

| `-ldflags` | build mode | binary | `.deb` | `lintian` E and W |
|---|---|---:|---:|---|
| none | exe | 19,495,376 | 8,478,076 | `E: unstripped-binary-or-object`, `W: hardening-no-pie` |
| `-s -w` | exe | 14,138,408 | 4,380,100 | `W: hardening-no-pie` |
| none | pie | 20,903,112 | 8,618,768 | `E: unstripped-binary-or-object` |
| **`-s -w` + pie — what ships** | | **15,545,600** | **4,522,296** | **no E, no W** |

(Those four are one build set, taken minutes after the package §5 ran against,
and they differ from §1's figures by a few kilobytes. That is not a
measurement error: other packages were committing into this tree while these ran,
so the source moved between the two sets. Each set is internally consistent,
and §1's is the one the twenty rows were run against.)

Read it in one direction and it says the release is clean. Read it in the other
and it says something more useful: **an `E` or a `W` reported against this
package is a statement about which flags the reporter built with**, and the
first question to ask is not "what changed in the packaging" but "what was the
build command". That question had a wrong answer written down in this
repository until this round; §9 has it.

Two rows needed thought rather than a command, and both are recorded here
rather than quietly adjusted.

### Row 16 — the row that failed twice, for two different wrong reasons

Row 16 is "the binary is not linked against libcap or libpolkit and makes no
`setuid`/`setgid`/`seteuid`/`capset` call", checked with `objdump -T` / `nm -D`
and `go list -deps .`. It has failed twice on a binary that was fine, and the
two failures are worth keeping side by side: the first was a check looking at
something every build of this product has, and the second was a check looking
for evidence the shipped build does not carry.

#### The first failure: nine imports every cgo Go binary has

The naive `nm -D` check fails:

```
$ nm -D --undefined-only usr/bin/debark-gui | grep -Ei 'set(uid|gid|euid|egid|reuid|regid|resuid|resgid|groups)@'
  U setegid@GLIBC_2.2.5      U setresgid@GLIBC_2.2.5
  U seteuid@GLIBC_2.2.5      U setresuid@GLIBC_2.2.5
  U setgid@GLIBC_2.2.5       U setreuid@GLIBC_2.2.5
  U setgroups@GLIBC_2.2.5    U setuid@GLIBC_2.2.5
  U setregid@GLIBC_2.2.5
```

**That is every cgo-enabled Go binary on Linux, not this one.**
`runtime/cgo/linux_syscall.c` compiles nine `_cgo_libc_set*` wrappers that the
`syscall` package calls when it must change credentials on every thread. The
control experiment — a Go program whose entire body is one call to an empty C
function — imports the identical nine:

```
$ cat main.go
package main
/*
#include <unistd.h>
static void nothing(void) {}
*/
import "C"
import "fmt"
func main() { C.nothing(); fmt.Println("hi") }

$ CGO_ENABLED=1 go build -o ctl . && nm -D --undefined-only ctl | grep -Ei 'set(uid|gid|groups|res|re)'
  U setegid@GLIBC_2.2.5 … U setuid@GLIBC_2.2.5     # the same nine
$ nm ctl | grep _cgo_libc_set
  00000000004a08c0 T _cgo_libc_setegid … 00000000004a0a40 T _cgo_libc_setuid
```

and the shipping binary carries the same nine wrappers:

```
$ nm usr/bin/debark-gui | grep _cgo_libc_set
  00000000009bb1a0 T _cgo_libc_setegid … 00000000009bb320 T _cgo_libc_setuid
```

The GUI links GTK and cannot be `CGO_ENABLED=0`, so it has them. **A dynamic
symbol import is not a call.**

#### The second failure: the wrapper match cannot see a stripped binary

The first fix for row 16 was to match each setuid-family import against its
`_cgo_libc_<name>` wrapper in the binary's own symbol table. That worked, and
then the release work hardened the link with `-s -w` — which is where the
wrappers live:

```
nm <pie, unstripped>  | grep -c _cgo_libc_set   ->  9
nm <pie, stripped>    | grep -c _cgo_libc_set   ->  0
```

So the shipped package scored **19/20 on its own acceptance test** while an
unstripped hand build scored 20/20 — the row failing for want of evidence, not
because it found anything. `verify-deb.sh` said so in its own words:
*"binary is stripped, so the origin of setuid cannot be corroborated here"*.
`docs/release.md` §4.7 decided, correctly, to keep `-s -w`, and recorded what
was owed to this script.

**A row that cannot pass on the artefact that ships gets waived, and a waived
row protects nothing.** So row 16's corroboration is now built out of things
`-s` cannot remove. Four checks, and every one of them can fail:

| | Check | Survives `-s -w`? |
|---|---|---|
| 16a | `DT_NEEDED` names no `libcap`, `libcap-ng` or `libpolkit` | yes — dynamic |
| 16b | no capability or polkit symbol of any kind imported | yes — dynamic |
| 16c | the privilege-related dynamic imports are **exactly** the nine, no more and no fewer | yes — dynamic |
| 16d | each of the nine is reached from **exactly one** direct call site: the single wrapper | yes — PLT stubs are named from the dynamic relocations |
| 16e | the `_cgo_libc_*` wrapper match, when a static symbol table is present | no — extra evidence on an unstripped build, never the row's only evidence |

16c is the suggestion `docs/release.md` §4.7 made, and it is right but it is
not sufficient on its own, which is worth saying because the obvious reading is
that it is. Its watch list is deliberately **wider** than the nine — it also
holds `setfsuid`, `setfsgid`, `initgroups`, every `cap_*` and every `polkit_*`
— so a *new* privilege import fails the row. What it cannot see is a call to
one of the nine, because that import is already there for the runtime's sake.
That is exactly the case the old wrapper match could not see either: the
wrappers are unconditionally linked, so a C `setuid()` call was "matched" by
the runtime's own wrapper and passed. **16d is the check that closes it**, and
it is the one thing about this row that is genuinely stronger than before.

16d fails only on *evidence of an extra caller*, never on absence of evidence.
If a future binutils annotates the disassembly in a shape it does not
recognise, the counts come back zero and the row says "uncorroborated" rather
than "failed" — because a row that goes red on a toolchain upgrade is a row
that gets waived, and that is the failure this whole subsection is about.

#### The negative controls — proof the row can still fail

A row that cannot fail is worse than a missing row, so this was not argued, it
was run. Three packages were built by unpacking the real release-path `.deb`,
replacing `/usr/bin/debark-gui` with a **stripped, PIE** cgo binary of the
same shape, and repacking. Each was then put through the real
`packaging/verify-deb.sh`:

| Control | What its C does | Row 16 |
|---|---|---|
| `ctl-setfsuid` | `setfsuid(1000)` — a tenth credential symbol | **FAIL** — *privilege import(s) beyond the nine runtime/cgo wrappers: setfsuid* |
| `ctl-libcap` | `cap_get_proc()`, linking `libcap` | **FAIL** — *DT_NEEDED includes libcap…; imports a capability- or polkit-manipulating symbol; privilege import(s) beyond the nine: cap_free cap_get_proc* |
| `ctl-setuid` | `setuid(0)` — one of the nine, called from C | **FAIL** — *setuid is called from 2 places, not just the runtime/cgo wrapper* |

The third is the interesting one. Its dynamic import set is **exactly** the
nine, so 16a, 16b and 16c all pass it; the whole finding is 16d's, from the
call-site count on a stripped binary — one caller for each of the other eight,
two for `setuid`. It is also the control the *old* row would have passed
outright, wrapper match and all.

The positive control is the shipped package itself, which passes all four on
all three supported releases, and the unstripped comparison build, which
additionally exercises 16e:

```
$ objdump -d usr/bin/debark-gui | count direct call/jmp sites per <name@plt>
  setegid 1   setgroups 1   setresgid 1   setreuid 1
  seteuid 1   setregid 1    setresuid 1   setuid 1   setgid 1
```

#### The `go list -deps` half

It needs the source tree, which a `.deb` does not carry, so it is run at build
time. Across the **358 packages that link into the shipping binary**, ignoring
`_test.go` files, which never link, **exactly one package matches at all**:

```
$ go list -tags "desktop,production,webkit2_41" -deps . | wc -l
358
$ # for each package's non-test .go files, grep for
$ # (syscall|unix|os).Set{uid,gid,euid,egid,reuid,regid,resuid,resgid,groups}(,
$ # capset, capget, cap_set_proc, cap_get_proc, cap_from_text, CAP_SYS
--- golang.org/x/sys/unix
syscall_linux.go:2082:  return syscall.Setuid(uid)
syscall_linux.go:2086:  return syscall.Setgid(gid)
syscall_linux.go:2090:  return syscall.Setreuid(ruid, euid)
…plus the generated CAP_* constant table in zerrors_linux.go
packages with a hit: 1
```

**And every one of those lines is a definition, not a call.**
`golang.org/x/sys/unix` declares thin `Setuid`/`Setgid`/… wrappers over the
`syscall` package and generates a `CAP_*` constant table; the grep looks for
callers everywhere in the closure and finds none, in this application, in
`debark`, or in Wails. **No package that links into this binary calls any
credential-changing function.**

An earlier revision of this section reported the single hit as "a constant
table". That was the visible half; the wrapper definitions are the other half,
and naming both is the difference between a claim a reader can check and one
they have to take on trust.

### Row 19 — what "no diff" was actually measured against

The row names `getent group`, `systemctl list-unit-files` and
`find / -perm /6000`. `verify-deb.sh` diffs those and four more, before the
install, while installed, and after removal:

`getent group`, `getent passwd`, `systemctl list-unit-files`, the on-disk unit
and autostart directories (`/etc/systemd`, `/lib/systemd`, `/usr/lib/systemd`,
`/etc/init.d`, `/etc/xdg/autostart`), `find / -xdev -perm /6000 -type f`,
`/etc/sudoers` + `/etc/sudoers.d`, and every file under `/etc/polkit-1`,
`/usr/share/polkit-1`, `/etc/udev/rules.d`, `/lib/udev/rules.d`,
`/etc/dbus-1`, `/usr/share/dbus-1`.

The on-disk unit directories are there because `systemctl list-unit-files` is
vacuous in a container with no systemd package installed — it reads unit files
off disk and needs no running init, but it does need the binary. The test
images install `systemd` so the row's literal command carries weight
(234 unit files on the noble image); the filesystem diff is the belt to its
braces, and the transcript prints both counts so a reader can see which one
did the work.

The result is stronger than the row asks for: **no diff across install and
removal, and no diff while the package was installed either.**

### Row 20 — the screenshot, and the hang that had to be fixed first

Started as `debark` (uid 1001) with `setpriv --clear-groups`, on Xvfb, on a
stock Ubuntu image where `dpkg-query` finds no `sudo`, no `policykit-1` and no
`pkexec`. The window titled `Debark` appeared, was activated and photographed;
the captures have 3,053 (noble), 3,192 (resolute) and 3,053 (trixie) distinct
colours, which is how the transcript distinguishes "it started" from "it is a
black rectangle" — a mapped but unpainted WebKit window photographs as one flat
colour, and that failure has cost this project time before.

**This row could hang indefinitely, and now it cannot.** A container has no
session bus, so GTK falls back to `dbus-launch --autolaunch`, which blocks;
`docs/release.md` §4.7 observed it at over ten minutes with no timeout anywhere
in the script. Two changes, and the second matters more than the first:

- **The row starts its own session bus** — `dbus-daemon --session --fork`,
  started *as the unprivileged user*, because a bus started by root refuses a
  connection from uid 1001 and the fallback comes straight back. Its address
  and pid are read from explicit file descriptors rather than from output
  ordering. With it, the row completes in seconds. This is not making the row
  easier: a real desktop has a session bus, and this row is about privilege.
- **Everything is bounded.** The application runs under `timeout`
  (`ROW20_TIMEOUT`, 90 s), every `xdotool`/`xprop`/`import` call is under
  `timeout`, and the X server, the window manager, the bus and the app are all
  tracked **by pid** and torn down by pid on the way out — never by name, on a
  machine with other people's containers on it. So the next person to hit a
  stuck start gets a FAIL with a transcript rather than a terminal that never
  returns, which is the difference between a defect that is reported and one
  that is waited out.

One incidental fix went with it: `mktemp -d` gives 0700, so the unprivileged
user could not traverse the work directory and the `XDG_RUNTIME_DIR` the row
hands it was unreachable. It is 0711 now.

The only output on stderr is the AT-SPI accessibility-bus warning, WebKit's
`JSC_SIGNAL_FOR_GC` notice and the software renderer's two `libEGL` DRI3
lines, all three expected in a container.

**The `.desktop` file's `StartupWMClass` was corrected by this measurement.**
`main.go` sets `options.Linux.ProgramName = "debark"` and comments that this
is "what a Linux desktop groups windows and matches .desktop files by". The
running app disagrees:

```
$ xprop -id <window> WM_CLASS
WM_CLASS(STRING) = "debark-gui", "Debark-gui"
```

GTK took `WM_CLASS` from the executable name, not from `g_set_prgname`. So
`StartupWMClass=debark-gui`, which is what the package installs as. Had the
value been taken from the comment rather than from `xprop`, the installed
application would have lost its icon in the dock and the alt-tab list, and
nothing in the checklist would have caught it. Reported: `main.go`'s comment
overstates what `ProgramName` does.

### Other packaging checks, outside §5

```
$ desktop-file-validate usr/share/applications/debark-gui.desktop
  hint: value item "PackageManager" in key "Categories" can be extended with
        another category among the following categories: Settings
```
Clean. The hint is advisory; `System` is already the required main category
and `Settings` would be wrong for a bundler.

```
$ appstreamcli validate --pedantic --explain usr/share/metainfo/io.github.inferops.Debark.metainfo.xml
W: url-not-reachable  https://github.com/inferops/debark/tree/main/gui  (×2)
W: url-not-reachable  https://github.com/inferops/debark/issues
P: cid-contains-uppercase-letter  io.github.inferops.Debark
Validation failed: warnings: 3, pedantic: 1
```
No errors. The three warnings are the validator making its own HTTP requests
to the homepage, bug tracker and VCS URLs, which are not published yet; the
metadata is not wrong, the repository is not public. The pedantic note fires
on every reverse-DNS id with a capitalised final segment — `org.gnome.Calculator`
gets it too — and the convention is deliberate.

**This transcript has been rewritten for the id and URL corrections, not
re-run.** The original run was against the pre-monorepo file, whose component
id still carried the old organisation and whose three URLs were one unreachable
address rather than two. The verdict is unchanged — same three
unreachable-URL warnings, same pedantic note, no errors — but nobody has
re-executed `appstreamcli` since the rename.

`releases-info-missing` does not appear above because `build-deb.sh` injects a
`<release>` element for the version it is packaging. The committed metainfo
file has no `<releases>`: the only honest content for it is the version being
built, and a placeholder in a committed file is a file that is wrong on disk.
**A build that installs the committed file verbatim — goreleaser/nfpm does —
will ship without a release history and will see `releases-info-missing` as a
warning.** Recorded rather than hidden; see §7.

---

## 5. Building it

```bash
# 1. the binary, with EXACTLY the flags .goreleaser.yaml uses
go run ./hack/copyfrontend
CGO_ENABLED=1 go build -tags "desktop,production,webkit2_41" \
    -trimpath -buildmode=pie \
    -ldflags "-s -w -X main.version=$VERSION" -o /tmp/debark-gui .

# 2. the package
packaging/build-deb.sh --binary /tmp/debark-gui --version "$VERSION"
```

**Every flag in step 1 is load-bearing and two of them used to be missing from
this command.** `desktop,production` are what make the binary run at all;
`webkit2_41` selects the only WebKitGTK ABI a supported release ships;
`-buildmode=pie` clears `W: hardening-no-pie` and `hardening-no-bindnow`; and
`-s -w` clears `E: unstripped-binary-or-object`. Drop the last two and you
build a package the release never produces, `lintian` reports an error against
it, and the error is real about your build and false about the product. That is
not hypothetical — it happened, and §9 records it. The same command is in
`packaging/build-deb.sh`'s header, and the two must not drift apart again.

`build-deb.sh` needs `dpkg-deb`, `dpkg-shlibdeps`, `gzip` and coreutils. It
needs no goreleaser, no nfpm, no debhelper and no `dpkg-buildpackage`, and it
runs unprivileged — `dpkg-deb --root-owner-group` sets `root:root` without
`fakeroot`. That is deliberate twice over. It means anyone with a checkout and
a Debian-family machine can build the package and run §5 against it, and a
checklist that can only be run by the release pipeline is a checklist nobody
audits. And it means the release pipeline can call *this* script rather than
reimplementing it, which is what §7 records — one producer, so the artefact
under the signature is the artefact the twenty rows were run against.

It refuses to package a binary that is not the desktop app. Without
`desktop,production` a Wails binary compiles and then refuses to run — a
failure invisible to §5 rows 1–19 that would only surface at row 20 — so
`build-deb.sh` greps the ELF for the string the wrong binary prints, and
checks that `libwebkit2gtk-4.1.so` is in `DT_NEEDED` rather than the 4.0 ABI.

---

## 6. Reproducibility — measured

`build-deb.sh` clamps every file's mtime to `SOURCE_DATE_EPOCH`, gzips the
changelog with `-n` so no timestamp and no original filename lands in the
member header, and lets `dpkg-deb` use the same variable for the `ar` member
headers.

The release work proved the Linux binary byte-reproducible under a fixed C
toolchain and recorded that it could not measure the `.deb`, because the `.deb` did not
exist yet. **It does now, and it is byte-identical.** Two runs, seconds apart,
same binary, same fixed `SOURCE_DATE_EPOCH`, through exactly the environment
interface the release uses:

```
$ export DEBARK_GUI_BINARY=/out/debark-gui DEBARK_GUI_VERSION=0.1.0          DEBARK_GUI_ARCH=amd64 SOURCE_DATE_EPOCH=1788700000
$ DEBARK_GUI_OUTDIR=/out/dist1 packaging/build-deb.sh
$ sleep 3
$ DEBARK_GUI_OUTDIR=/out/dist2 packaging/build-deb.sh
$ sha256sum /out/dist{1,2}/*.deb
915eee1d0408a18e774bb4d066698aad6c2f88624b421eed697269794d0f2d28  /out/dist1/debark-gui_0.1.0_amd64.deb
915eee1d0408a18e774bb4d066698aad6c2f88624b421eed697269794d0f2d28  /out/dist2/debark-gui_0.1.0_amd64.deb
$ cmp /out/dist1/*.deb /out/dist2/*.deb && echo BYTE-IDENTICAL
BYTE-IDENTICAL
```

`SOURCE_DATE_EPOCH` is load-bearing, not decoration: without it the script
falls back to the commit date of the last change under `packaging/`, and if
the tree has no git it falls back to wall clock, at which point two runs
differ.

**Re-verified after the changelog-name change in §9**, because a change to
which file the package installs is exactly the kind of change that can quietly
introduce a timestamp. It did not. Two runs each, seconds apart, from the
release-path binary with `SOURCE_DATE_EPOCH=1757000000`:

```
201344312a5c77a9824fa381a47987fed49885e7bbdc9ad80b854ed1736e924e  debark-gui_1.2.3_amd64.deb
7b721796e43cf6739a65915789a47b8c1583a34c28e6e381a485ea52619c2e3a  debark-gui_1.2.3-rc1_amd64.deb
```

both BYTE-IDENTICAL across their two runs. The two digests differ from each
other because the packages differ — different version, and one carries
`changelog.gz` where the other carries `changelog.Debian.gz` — which is the
point.

**That is packaging reproducibility, and it is the smaller half.** The binary
inside is built with cgo and an external linker, so its reproducibility is a
question about the C toolchain and the system headers on the build machine,
not about this script — `-trimpath` removes the build paths and `-ldflags -X`
is deterministic, but nothing here establishes that two machines with
different GCC or glibc produce the same ELF. The release work measured that half
and owns the claim that goes in the release notes. This half says only that the
packaging step adds no nondeterminism of its own, which is now a number
rather than an intention.

---

## 7. For the release work — the interface, confirmed

**`.goreleaser.yaml` has no `nfpms:` block, and that is the right call.** The
`.deb` has exactly one producer, `packaging/build-deb.sh`, run from the Linux
build's post hook and picked up by `checksum.extra_files` and
`release.extra_files` so that it lands under the cosign signature. The reason
is the one the release work gave and it is worth restating: `verify-deb.sh` proves
`docs/security-review.md` §5 against a *built package*, so what is proven must
be what ships. Two producers would mean the artefact under the signature is
not the artefact the twenty rows were run against, and §5 would be describing
a package nobody released.

`build-deb.sh` accepts the interface the release expects. Every input can be
given as a flag or as an environment variable; the flag wins.

| environment variable | flag | default |
|---|---|---|
| `DEBARK_GUI_BINARY` | `--binary` | required |
| `DEBARK_GUI_VERSION` | `--version` | derived from git (§1) |
| `DEBARK_GUI_ARCH` | `--arch` | `dpkg --print-architecture` |
| `DEBARK_GUI_OUTDIR` | `--out` | `build/deb` |
| `SOURCE_DATE_EPOCH` | — | commit date of the last change under `packaging/` |

The script writes **exactly one** `*.deb` into the output directory, prints
its path on stdout, and **fails if a second one is there**. That is checked,
not assumed, because the release identifies the package by glob for the
checksum file and therefore for the signature — a stray second `.deb` in
`dist/` would otherwise be signed-adjacent and unnoticed. It clears its own
previous output (`debark-gui_*.deb`) and touches nothing else in the
directory, because on the release path that directory is goreleaser's `dist`
and is full of other artefacts. Demonstrated:

```
$ ls /out/guard
someone-elses_1.0_amd64.deb   debark-gui_0.1.0_linux_amd64.tar.gz
$ DEBARK_GUI_OUTDIR=/out/guard packaging/build-deb.sh
build-deb.sh: expected exactly one .deb in /out/guard, found 2:
/out/guard/debark-gui_0.1.0_amd64.deb
/out/guard/someone-elses_1.0_amd64.deb
build-deb.sh: the release identifies the package by glob; more than one is ambiguous
$ echo $?
1
```

Two consequences of there being no `nfpms:` block, both good and both worth
writing down so nobody "fixes" them later:

- The **`Depends` cannot drift**, because there is no second place to write
  them. §1 explains why a hand-written list would be wrong; with one producer
  the question does not arise.
- The **metainfo `<releases>` element is correct in the shipped package**,
  because `build-deb.sh` injects it for the version being built. The
  `releases-info-missing` warning noted in §4 would only appear if something
  installed the committed file verbatim, which nothing now does.

### If a second producer is ever added

The inputs are at the agreed paths and none of them needs renaming: the source
basename and the installed basename are the same string in every case, so an
nfpm `contents:` entry would be a straight copy.

| source | installed |
|---|---|
| `packaging/debark-gui.desktop` | `/usr/share/applications/debark-gui.desktop` |
| `packaging/copyright` | `/usr/share/doc/debark-gui/copyright` |
| `packaging/icons/hicolor/<size>/apps/debark-gui.png` | `/usr/share/icons/hicolor/<size>/apps/debark-gui.png` |
| `packaging/metainfo/io.github.inferops.Debark.metainfo.xml` | `/usr/share/metainfo/io.github.inferops.Debark.metainfo.xml` |

1. **The app id is `io.github.inferops.Debark`.** It is the AppStream
   component id and the metainfo filename, and nothing else. The desktop
   entry, the icon lookup key and the binary are all `debark-gui`.
2. **Do not add a `postinst`.** nfpm will not add one on its own; if a
   `scripts:` block is ever added for `update-desktop-database` or
   `gtk-update-icon-cache`, it breaks §5 row 3, and the dpkg triggers already
   do the work. See §1.
3. **`Depends` would have to be derived, and nfpm cannot.** It has no
   `dpkg-shlibdeps` equivalent, so a `depends:` list in the release config
   would be a second source of truth that drifts the first time an import
   changes. This is the strongest single argument for the current one-producer
   arrangement.
4. **The committed metainfo has no `<releases>` element** and nfpm would
   install it verbatim, so `appstreamcli validate` would report
   `releases-info-missing` (a warning). `build-deb.sh` injects the element for
   the version it is packaging.

Also for the release work, and reported separately: the `WEBKIT_TAG` comment in
the `Makefile` is wrong about Ubuntu 22.04 and Debian 12, which ship both
WebKit ABIs (§2).

### Build-time tools, and why each is allowed

`docs/dependency-review.md` is the release work's file, so the justification
lives here to be merged there. None of these is a runtime dependency; none ships in
the package; none is a Go module and none is an npm package. All three are
Debian's own packaging tools, present in every Debian-family build image.

| Tool | Package | Why | What its compromise would mean |
|---|---|---|---|
| `dpkg-deb` | `dpkg` | Builds and unpacks the `.deb`. There is no way to produce a Debian package without it, and it is already on every machine that can install one. | It is the same binary that installs the package on the target, so a compromise is already total on any Debian machine. Adding it as a build tool widens nothing. |
| `dpkg-shlibdeps` | `dpkg-dev` | Derives `Depends` from the built ELF. The alternative is a hand-written list, which is a second source of truth for a value that changes with the code. | It could emit a too-loose dependency, letting the package install where it cannot run, or a too-tight one, blocking a valid install. It writes a control field; it does not touch the payload. Every value it produced is printed by `build-deb.sh` and is in §1, so a wrong answer is visible rather than silent. |
| `lintian` | `lintian` | §5 row 18 names it. It only reads the package. | It could report a clean package as dirty or vice versa. Row 18 is one of the eighteen rows §5.2 already says are individually defeatable; rows 19 and 20 do not involve it. |

`appstreamcli` (`appstream`) and `desktop-file-validate` (`desktop-file-utils`)
are used for the §4 checks outside §5, on the same terms: read-only
validators, nothing shipped.

`appstreamcli validate` makes outbound HTTP requests to the URLs in the
metadata to check they resolve. That is the validator's behaviour, not the
package's and not the application's; it is a developer-machine tool and is
never run at install time or at run time. Rule 3 is untouched — the requests
go to `github.com`, and nothing in the shipped artefact contacts anything.

---

## 8. The Windows installer

`build/windows/installer/README.md` holds the full record. In short:

- `wails_tools.nsh` was committed unchanged from `wails init` and was
  **byte-for-byte identical** to the upstream Wails v2.12.0 template
  (verified by diff against
  `pkg/buildassets/build/windows/installer/wails_tools.nsh` in the module
  cache). `docs/security-review.md` §6.6 found that it sets
  `REQUEST_EXECUTION_LEVEL "admin"` and silently runs Microsoft's *online*
  WebView2 bootstrapper.
- **It has been deleted, not fixed**, because a fix there cannot survive:
  Wails v2.12.0's `pkg/commands/build/nsis_installer.go` calls
  `ReadOriginalFileWithProjectDataAndSave` on it, which rewrites it from the
  template on every `wails build --nsis`. Wails' own `.gitignore` template
  lists the file for that reason. **`.gitignore` should gain
  `build/windows/installer/wails_tools.nsh`; that file is not this package,
  so it is reported rather than edited.**
- **`project.nsi` has been rewritten and kept**, because it is the one file in
  the pair Wails leaves alone (`ReadFile`: written only if absent), and
  because deleting it would not remove the problem — the next `wails build
  --nsis` on a tree with no `project.nsi` restores the stock template,
  administrator and all, with nothing left to say why that is wrong. It now
  sets `REQUEST_EXECUTION_LEVEL "user"` before the include, installs
  per-user under `$LOCALAPPDATA\Programs`, **detects WebView2 and tells the
  operator where to get it instead of installing it**, and registers the
  uninstaller under `HKCU` because `wails.writeUninstaller` writes `HKLM`
  unconditionally and a non-elevated install would silently leave no
  uninstall entry.

Proved by compiling both, on Ubuntu 24.04 with `makensis`, against the same
regenerated `wails_tools.nsh`:

```
$ makensis -DARG_WAILS_AMD64_BINARY=.../debark-gui.exe project.nsi
$ strings -a build/bin/debark-gui-amd64-installer.exe | grep -io 'requestedExecutionLevel level=."[a-zA-Z]*."'
```

| script | `requestedExecutionLevel` in the embedded manifest |
|---|---|
| upstream Wails `project.nsi`, unchanged | `requireAdministrator` |
| this `project.nsi` | `asInvoker` |

and `strings -a` on the installer built from this script finds no
`MicrosoftEdgeWebview2Setup` and no `webview2bootstrapper`.

One correction to §6.6, reported to the low-severity security follow-up: the
review says "the Wails build fetches an executable over the network and embeds
it in the installer". It does not. `internal/webview2runtime/webview2installer.go` carries
`//go:embed MicrosoftEdgeWebview2Setup.exe`, so the bootstrapper is embedded
in the `wails` CLI itself and written out from there — no build-time fetch.
The network fetch is at *install* time, by the bootstrapper, which is the
online installer. The finding stands; only that one sentence is wrong.

**No Windows installer is produced by anything in this repository.** Nothing
in `Makefile`, `.github/workflows/` or the release configuration passes
`--nsis`. Windows is built and tested, not supported.

---

## 9. What is not done

- **No `arm64` package.** `build-deb.sh` takes the architecture from
  `dpkg --print-architecture` and would produce a correct `arm64` package on
  an `arm64` builder, but none was built and none was tested. §5 has been run
  on `amd64` only.
- **No `scalable/apps/*.svg` icon**, because there is no vector source in the
  tree (§1).

Three entries that used to be here are gone, and each was wrong in a different
way. They are written out rather than deleted, because "what is not done" is
the section of a record most likely to be read years later by someone deciding
whether to trust the rest of it.

- ~~**No man page.**~~ **Done.** `build-deb.sh` installs
  `/usr/share/man/man1/debark-gui.1.gz` as of `08323d5`, and `lintian` no
  longer reports `no-manual-page`. `/usr/share/man` is the sixth prefix §5
  row 12 permits and §1 lists it.
- ~~**`hardening-no-pie` (W): recommended, not done.**~~ **Done, by the
  release track.** `.goreleaser.yaml` builds Linux with `-buildmode=pie`
  (`f9cf4b6`); the tag is gone, `hardening-no-bindnow` went with it because
  Go's pie link is `-z now`, and reproducibility was **re-proved with the flag
  on** rather than assumed to survive it. `docs/release.md` §4.7 has the cost:
  +1.4 MB on the binary, +220 dynamic relocations, no measurable cold start.
- ~~**`unstripped-binary-or-object` (E): Go binaries conventionally keep their
  symbol table, and `go version -m` provenance is worth more than the
  megabytes.**~~ **The reasoning did not hold, and neither did the premise.**
  Three things, all measured, all in `docs/release.md` §4.7:
  1. **The release has always been stripped.** `-s -w` has been in
     `.goreleaser.yaml`'s `ldflags` from the start, matching `..`'s.
     This bullet described a build the release never produced.
  2. **`go version -m` still prints the whole module graph on the stripped
     binary** — Go version, main module path, and every `dep`/`=>` line
     including `github.com/inferops/debark v0.0.0 => .. (devel)`.
     Build info lives in its own section, not in the symbol table. That was
     the load-bearing argument for keeping the symbols and it does not bear.
  3. **Panic tracebacks are identical**, function for function, file for file,
     line for line, because they come from the pclntab, which `-s -w` does not
     remove. What is actually lost is DWARF, so `gdb` on a released binary is
     degraded — a trade this project had already made.

  **So where did the `E` come from?** From `packaging/build-deb.sh`'s own
  header, which told the reader to build with `-tags … -trimpath` and nothing
  else. Someone followed it, produced an unstripped non-PIE binary, packaged
  it, and reported the tag. The `E` was true of that package and false of the
  product, and the four-build table in §4 shows every tag attributable to a
  flag. **The header now carries the release's exact flags and says why each
  one is there**, and so does §5.
- **Ubuntu 25.10 was not exercised.** Its archive metadata was read (§2) but
  no install and no start was attempted there.
- **`desktop-file-utils` is not installed on the `debian:13` test image**, so
  its transcript shows only the `hicolor-icon-theme` trigger firing where the
  Ubuntu ones show both. That is a property of a minimal container, not of the
  package: the `.desktop` file is installed correctly either way, and the
  desktop database is updated by the trigger on any machine that has a desktop
  at all. Worth knowing before someone reads the two transcripts side by side
  and thinks the package behaves differently on Debian.
- **Two Go tests were reported to fail inside any Docker container**:
  `internal/export`'s `TestLiveMountInfoParses` and `TestLiveListVolumes` —
  `/` is an overlay mount that `selectMounts` drops. **They pass now.**
  `go test ./...` in `debark-deb-build:noble` is green across every package,
  including `internal/export`. Recorded as observed-then-not-reproduced rather
  than removed, because whatever changed is not this package and nobody has
  named it. `gofmt -l .` is empty, `go build`, `go vet` and
  `golangci-lint run ./...` are clean, and nothing in this package touches a Go
  file.
- **Ubuntu 22.04 and Debian 12 are not supported** and the reason is measured,
  not assumed (§2). Producing packages for them is possible and is a release
  decision.

### Two things this package found and did not fully close

**1. Row 16's evidence and `-s -w` pull in opposite directions, and the tension
is real rather than resolved.** §4 records the rework: 16c and 16d corroborate
on a stripped binary and three negative controls prove they can fail. What is
*not* claimed is that the row is now as strong as it could be with symbols. It
is strictly stronger than the old wrapper match — 16d catches a C-level
`setuid()` call, which the wrapper match never could — but it rests on two
things a toolchain owns: that `runtime/cgo` compiles exactly these nine
wrappers, and that `objdump` names PLT stubs from the dynamic relocations. If
either changes, 16c fails loudly (correct: a person should look) and 16d goes
quiet (deliberate: reporting "uncorroborated" beats going red on a binutils
upgrade). **A future Go release that adds a tenth wrapper will fail this row,
and that is the design.** §5.2 says a row that cannot be satisfied is a design
conversation, not a waiver; this is where that conversation would start.

**2. A pre-release tag ships a package `lintian` calls broken — the `E` is
fixed, the version ordering is not, and the second half is not this file's to
fix.**

`.github/workflows/release.yml` fires on `v[0-9]+.[0-9]+.[0-9]+-*` as well as
on the plain form, so `v1.2.3-rc1` is a releasable tag. dpkg splits a version
at its **last** hyphen, so a hyphen turns a native package into a non-native
one, whose changelog must be `changelog.Debian.gz` rather than `changelog.gz`.
Reproduced from **one binary at three versions**, before the fix:

```
$ lintian debark-gui_1.2.3_amd64.deb
  (nothing)                                                    exit 0
$ lintian debark-gui_1.2.3-rc1_amd64.deb
E: debark-gui: debian-changelog-file-missing-or-wrong-name   exit 2
$ lintian debark-gui_1.2.3-SNAPSHOT-a1b2c3d_amd64.deb
E: debark-gui: debian-changelog-file-missing-or-wrong-name   exit 2
```

`1.2.3-SNAPSHOT-<sha>` is not hypothetical either: it is what `make snapshot`
produces. **Fixed in `build-deb.sh`**, which now picks the changelog name from
the version — the rule is dpkg's, not a preference — and says out loud, at
build time, that it has done so. After the fix all three are `exit 0` with no
tag, and both pre-release packages remain byte-reproducible (§6).

**What is not fixed is the ordering, and it is a release decision rather than a
packaging one.** A Debian revision sorts *above* the bare upstream version, so
`1.2.3-rc1` is **newer** than `1.2.3` to dpkg and a release candidate shadows
the release it precedes — the exact failure §1's `0.0.0~git…` scheme uses `~`
to avoid, reappearing through a door that scheme did not cover. §1 also says
this package is native with no Debian revision, which a hyphenated tag silently
makes false.

The fix is to map a semver pre-release's `-` to Debian's `~` (`1.2.3~rc1`,
which sorts below `1.2.3` and stays native). `build-deb.sh` does **not** do it,
deliberately: that changes the version string the release publishes, and
`.goreleaser.yaml` passes `{{ .Version }}` straight through. Rewriting a
release's version inside a packaging script, without the release configuration
saying so, is the kind of silent divergence between the two that produced the
`unstripped` bullet above. **Recommended to whoever owns the release's version
string; the `lintian` error it caused is closed either way.**
