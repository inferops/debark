# Debark — a guide for the operator

This is the operator's companion to the app. It covers what Debark is for, the
three stages it takes you through, and what to do when something fails. The
product definition and the permanent do-not-build list are in
[`../README.md`](../README.md) and are not repeated here.

The workflow was updated on 2026-09-07 against the desktop simplification
implementation. It describes the controls and state rules in the current
source; current native validation and remaining limits are recorded in
[`accessibility.md`](accessibility.md) and [`performance.md`](performance.md).
Earlier packaging and failure evidence came from the Linux GTK3/WebKit2GTK
build, including an 85,565-package Ubuntu catalogue and a real 59-package
bundle. Those measurements are historical evidence, not a fresh measurement
of every redesigned view.

---

## What it is for

You need software on a machine that has no internet connection. The usual
approach is to run `apt-get install --download-only` on a machine that *does*,
and carry the `.deb` files over.

That does not work, and it fails in a way that looks like it worked. `apt-get`
on the online machine only downloads what is missing **on that machine**. Every
dependency the builder already has installed is silently skipped. The folder
fills up, the copy succeeds, and the failure happens on the offline machine,
hours later, as a wall of unmet dependencies — with no network to fix it from.

The other half of the problem is the blank file. Somebody hands you a
`packages.txt` to fill in and you know what you want the offline machine to
*do*, not which twelve binary package names produce it.

Debark solves both by asking a different question. You name the **target** —
the machine the bundle is for, not the one you are sitting at. Everything after
that is derived from the target: the package list you browse is the target's
own apt catalogue, and the closure that gets built is resolved by the target
release's own apt, so it contains exactly what that machine is missing and
nothing it already has. The output is a bundle folder: a real flat apt
repository plus a manifest — signed with your operator key, if you have one —
which the offline machine installs from as if it were an archive.

### Two names, one product

The window says **Debark**. The command line it drives is **`debark`**, and
you will see that name in the app — in System check's first row, in the
Command disclosure in Bundle, and in error messages.
They are not two tools you have to install separately in the sense that
matters: Debark is a front end, `debark` is the engine, and Debark cannot do
anything without it. The engine keeps its own name because it is a real
command-line tool that people use directly, on the far side and in CI.

### Where it runs

Debark runs on the **online builder machine** — the one with a network, where
you prepare the transfer. It never runs on the offline target and it does not
need to: on the far side the bundle is installed with the `debark` CLI, or
with plain apt against the bundle's repository. Nothing about the offline
machine changes because you started using this app.

---

## Getting it

Debark ships as a Debian package for Linux, which is the supported platform.
The package is **`debark-gui`** — one file of about 8 MB, with no maintainer
scripts at all, so nothing in it runs as root at install time. Install the
`.deb` published with the release:

```sh
sudo apt install ./debark-gui_<version>_<arch>.deb
```

apt pulls in GTK, the WebKitGTK runtime and the rest as ordinary dependencies.
Those are derived from the binary rather than written down, so they name the
libraries this build actually links. Nothing is installed outside `/usr/bin`,
`/usr/share/man`, `/usr/share/applications`, `/usr/share/icons`,
`/usr/share/metainfo` and `/usr/share/doc/debark-gui`; removing the package
leaves behind no user, group, service or permission. It appears in the desktop
menu as **Debark**, and `man debark-gui` is a short reference page — where
the cache lives, what the environment variables do, what the exit status means.

### Which releases it installs on

Established by attempting the install on each one rather than by reading a
compatibility table; the transcripts are in [`packaging.md`](packaging.md).

| Release | Installs and starts |
|---|---|
| Ubuntu 24.04 LTS | yes |
| Ubuntu 26.04 LTS | yes |
| Debian 13 | yes |
| **Ubuntu 22.04 LTS** | **no** |
| **Debian 12** | **no** |

**Ubuntu 22.04 and Debian 12 cannot install this package**, and the reason is
worth knowing because it is not the one people assume. It is not WebKit: both
of those releases carry WebKitGTK 4.1 perfectly well. It is the 64-bit `time_t`
transition. They predate it, so their GTK and GLib packages are still named
`libgtk-3-0` and `libglib2.0-0`, while a package built on a release that has
been through the transition depends on `libgtk-3-0t64` and `libglib2.0-0t64`.
apt says exactly that and refuses:

```text
debark-gui : Depends: libglib2.0-0t64 (>= 2.16.0) but it is not installable
               Depends: libgtk-3-0t64 (>= 3.21.4) but it is not installable
E: Unable to correct problems, you have held broken packages.
```

That is a clean refusal — nothing is half-installed and there is nothing to
undo. **What to do instead: use the `debark` command-line tool on that
machine.** That is not a consolation prize. `debark` is the whole engine,
it has no GUI dependencies of any kind, and everything this application does
is a `debark` command — the build screen will even show you the command. The
other option is to run Debark on a machine with a newer release and carry the
bundle: a bundle is an ordinary folder, so building it in one place and using
it in another is exactly what it is for.

### The one thing that is not a dependency

You also need `debark` itself installed and on your `PATH`. It is
deliberately **not** in this package's `Depends`: it is not published in any
distribution archive, so depending on it would make the package uninstallable
everywhere. A missing `debark` is the first thing System check
checks, and it names both places it looked.

Debark also builds and runs on Windows, and is not supported there — the
obstacle is `apt`, not the window. [`windows.md`](windows.md) is the measured
record of what does and does not work there; read it before you rely on it.

---

## Start on Target; use System check when needed

Debark opens on **Choose target**. A system check runs in the background;
healthy checks need no acknowledgement. The main menu contains **System
check**, **Copy existing bundle…**, **Appearance**, **About**, and **Quit**.
Versions and theme controls live there rather than in a permanent status bar.

System check describes the online builder's capabilities. Problems appear
with their remedies; healthy and optional checks are under **Details**. Use
**Recheck** after correcting a problem or **Copy report** when reporting it.
A usable native apt can build for its matching release; another target may
need a container even when this builder's overall report is healthy. The
actual build still validates the chosen target.

Actions that Debark can perform show the exact command and ask before running.
Commands requiring administrator rights are copyable instructions to run in
your own elevated terminal. System check's **Back** returns to the view that
opened it. You can keep choosing a target and preparing inputs wherever a
missing prerequisite does not prevent that work.

## The three stages

The header shows **Target → Packages → Bundle**. Copying is a contextual view
inside Bundle. Each view has one bottom action area, with Back returning to
the appropriate earlier view. Revisiting a view keeps the current session's
work. While a build or copy runs, navigation and viewing remain available;
conflicting target and selection edits explain why they are unavailable. The
header offers a return to the running job.

### Target: which machine is this bundle for?

Choose **Standard installation** or **Snapshot file**:

- **Standard installation** describes a default installation using
  Distribution, Version, Edition and Architecture. The available choices come
  from `debark`. Its baseline is an assumption about the offline machine;
  read the caveat if that machine differs from a default installation.
- **Snapshot file** uses a snapshot previously taken on the target with
  `debark snapshot create`. Choose the file in the native picker. Debark
  shows its filename, operating system, architecture and capture time. It
  describes the machine when captured, not necessarily its current state.

The standard-installation choice is the fresh-session default; Debark does
not infer the offline target from the online builder. Technical details hold
base seeds and exclusions, recommends policy, schema, digests and source
metadata. Validation problems stay beside the choice they affect.

Press **Continue** to commit the valid target and open Packages. Returning to
an unchanged target retains its cached package list and current selection.
Changing the target retains selected inputs and checks their catalogue
metadata again; unavailable packages remain visible for review.

### Packages: find and select what you need

After Continue, Debark shows **Loading packages…** while it downloads and
parses the target's package indexes. **Cancel** stops preparation. A ready
cache is reused. Cancellation and failure wait for an explicit **Retry**;
reopening the view does not repeatedly restart a failed download.

Start by typing a name or description. There is no search-submit button.
An empty query shows categories and **Browse all packages**, without an
applications-only filter. Query, category, scope and scroll position survive
an ordinary revisit. Rows show an application display name when available,
the exact package name, and a short summary.

Use the checkbox to select. With keyboard focus in the list, use arrows,
Home, End, Page Up and Page Down to move; **Space** toggles selection and
**Enter** opens **Details**. Details includes full description when available,
version, architecture, source, sizes and warnings. If the cache contains only
a summary, the view says so. Closing returns focus to the same logical row
while the target and query remain unchanged.

The always-visible **Add…** menu offers:

- **Paste list…** — preview package names, `name=version` entries, comments,
  duplicates and rejected lines before adding.
- **Add URL…** — a vendor `.deb` URL, with an optional expected SHA-256.
  Validation preserves the digest. The build downloads the file later.
- **Add local .deb…** — choose one or more files on this machine. The build
  reads them locally; selecting them uploads nothing.

**Selected (N)** is the selection count. When space permits, selected items
appear in a compact side pane. At narrow effective widths or enlarged text,
the same button opens a named dialog. An empty selection takes no permanent
pane space. Remove items directly, or Clear the selection; the bottom action
area offers one-step **Undo removal** or **Undo clear**, including after the
pane disappears. The next selection mutation or target change expires Undo.
Selection and Undo exist only for this session; they are not saved on quit.

Selection Details contains installed/download estimates for selected packages
only. They exclude dependencies and unknown vendor sizes. The build resolves
the actual dependency closure. Vendor, snap and unavailable-package warnings
remain with the affected input. Displayed vendor references conceal URL
credentials and query strings; Undo restores the original input internally.

Choose **Review bundle** when the selection is ready.

### Bundle: review, build and copy

Review the target and selection; their **Edit** actions return to the relevant
view. Choose an output folder with the native picker and optionally a bundle
name. A blank name means **Automatic name** until the build resolves it.

Signing stays visible:

- **Sign with a key** uses **Choose key…** to select a private-key file. The
  screen receives its reference, never its contents. **Manual key reference**
  accepts the supported GPG or plugin reference.
- **Create key…**, when available, runs the existing key-generation action
  after confirmation. After it finishes, use **Choose key…** and select the
  file named by that action; creating a key does not silently choose it.
- **Use configured default** delegates to `debark`'s configuration. It can
  produce an unsigned bundle if no default exists; inspect the actual result.
- **Unsigned bundle** is an explicit alternative. Signing failures never
  silently switch to it.

Recipients need the public key through an independently trusted route. A key
carried only on the same media as the bundle cannot establish who created it.
Unsigned verification on the offline side requires deliberate acceptance with
`--allow-unsigned`.

**Advanced options** holds format, software inventory (SBOM), recommends
policy, upgrades, index refresh, superseded-file retention and backend choice.
Changed options are summarized while collapsed. Refresh also enables upgrades;
keeping superseded files depends on refresh. Defaults remain a folder bundle,
SBOM on, target recommends policy and automatic backend.

Acknowledge responsibility for redistribution, then choose **Build bundle**.
The action explains missing prerequisites. Validation still runs when the
**Command** disclosure is closed. Command contains a copyable invocation with
URL credentials and query strings replaced by `REDACTED`; restore those values
before trying to run a copied command.

During a build, the view shows the current phase, elapsed time, meaningful
warnings and **Cancel**. Per-file byte progress appears when known. There is
no fabricated overall percentage: apt does not provide a total download count
before resolution. Logs and detailed counters are in Details. Leaving the
view does not cancel the build. Cancellation preserves completed downloads
for a later build and is shown as cancelled, without claiming a usable bundle.

**Bundle ready** shows the actual destination and signature status for that
run. Incomplete results stay visibly marked and list what needs attention.
**Open folder** opens the output; **Copy to drive…** carries that exact result
into the copy view even if the current draft later changes.

### Copy to a mounted destination

Copy is optional. To copy a bundle that already exists, choose **Copy existing
bundle…** from the main menu. The native source chooser opens without needing
a target or new build. Cancel returns to the menu's originating view. A copy
already running takes precedence and is shown instead of replacing its source.

Choose a mounted destination explicitly, even when only one drive is present.
Volume labels and mount paths distinguish destinations. An internal or other
non-removable destination requires explicit acknowledgement. You may also
choose a folder. Debark copies ordinary files; it does not format, partition,
write a raw device, or create bootable media.

The plan shows the source, actual destination path, required space, available
space and conflicts before writing. **Copy and verify** is the default.
Advanced contains the destination subfolder and **Skip verification**. Choosing
the latter exposes **Copy will be unchecked** and changes the action to
**Copy WITHOUT verifying**. A later copy starts with verification enabled.

Progress distinguishes copying, finishing writes and checking the copy.
The result records what was checked and the backend's caveats. Checking copy
integrity does not itself prove the bundle's signature. A failed, cancelled
or unchecked copy retains the backend's incomplete-state warning; do not use
it for installation. An external source's signing/completeness may be unknown,
and the view says so instead of assigning the current draft's properties.

After completion, use **Open folder** or **Copy to another drive**. Offline
installation guidance and copyable commands are in the result's Details.
The target installs with `debark install`, or apt against the bundle's
repository. Debark runs on the online builder, not on the offline target.

## Closing Debark

**Quit** and the native window close button use the same policy. During a
build or copy, **Keep working** is the default; **Stop and close** cancels
work and waits for cleanup. The window shows Stopping and stays responsive.
If cleanup takes too long, Debark stays open with a retryable message. Wait
for the operation to finish and try Quit again; it does not force-close.

If no job is running but the nonempty selection has not produced a complete
successful bundle at its current target and input revision, closing asks
before losing it. A completed matching build needs no such warning. Later
edits make it an unbuilt selection again. This confirmation does not save the
selection, and deliberate Stop and close does not add a second prompt.

---
## When a check fails

The rule that runs through all of these: an error in Debark is a sentence
saying what is wrong and a sentence saying what to do next, and both are always
present. If a screen shows you a bare exit code, that is a bug worth reporting.
Material failures stay visible with a remedy. Diagnostic evidence — command,
error class, event log and exact findings — is available in Details.

The failures below are the ones an operator actually meets. They come from
[`dev/error-catalogue.md`](dev/error-catalogue.md), which records earlier real
failures caused on purpose and what each must put in front of a person; that
document is the reference if you hit something not listed here.

### System check

**"The debark command-line tool was not found on this machine."**
Debark cannot do anything without the engine. The message names both places it
looked — your `PATH` and the folder beside the application. Install debark,
or put the binary on your `PATH`, then press **Re-check** on that row. It
re-probes; you do not have to restart the app.

**"The program this application is running as debark does not answer like
debark."** The configured path points at something else. The remedy is to
choose the right binary, or clear the setting so `PATH` is searched again.

**"This debark is older than this application."** An old engine has no
`snapshot list-bases`, so it cannot offer standard-installation targets. The
failed list offers Retry and the snapshot choice remains available. Install a
newer `debark` when you can; snapshot compatibility still depends on the
engine's validation of the selected file.

**"No container runtime is installed."** Only a problem if you need one. A
container is how `debark` runs the *target's* apt when this machine's own apt
cannot — a different distribution, or a different release. Building for this
machine's own release needs none. If you do need it, the row carries the
install command; it needs administrator rights, so copy it into a terminal you
have elevated and re-check the row afterwards.

**"…is installed and running, but this account is not permitted to talk to
it."** Your user is not in the `docker` (or `podman`) group. Add it, then log
out and back in — group membership does not take effect in a session that was
already open.

**"…is installed but its daemon is not running."** Start it and re-check.

**"…did not answer within the timeout."** The socket is wedged. Restart the
runtime and re-check; nothing else in Debark is waiting on it.

**Low disk space.** Advisory, not blocking. Free some space, or choose an
output folder on another drive when you get to the build screen.

### Choosing a target

**"…is not a debark snapshot"** — it does not begin with a zstd frame
header, or it is a directory with no `snapshot.json` in it. You picked the
wrong file. This appears next to the file field with the picker still open,
because it is a correction rather than a failed run. Pick the `.tar.zst` that
`debark snapshot create` wrote on the target machine, or switch to a stock
base.

**"That snapshot file is incomplete: it ends part-way through."** Different
failure, different fix. The file really is a snapshot; the copy did not finish.
Copy it again from the machine that made it, and let the copy complete before
ejecting the drive. A snapshot cut short looks exactly like this.

**"There is no snapshot file at …"** The path is gone. If it was on a
removable drive, plug the drive back in and wait for it to appear before
picking it again.

### Building the catalogue

The catalogue download needs to reach the archive named in the target's own
sources. If it cannot, check your network and — in particular, because this is
the usual answer on a corporate machine — your proxy settings. Readiness has an
**Archive network** row for exactly this question once a target is chosen.
Catalogues already on disk keep working; it is building a new one, and fetching
packages, that needs the host.

Package preparation starts after Target Continue and is cancellable. After
cancellation or failure, **Retry** starts a new attempt. Returning to the view
does not retry it automatically.

### Building the bundle

**"The target's package indexes could not be read, so even the base system's
own packages look missing."** This is the network, not your package choices,
and the message says so: the packages that failed belong to the base
definition, not to anything you picked. It is what a laptop on a captive-portal
wifi produces. Check that this machine can reach the archive, check the proxy
settings, and build again — **Retry** is the right action here.

**"The build was told to sign with the key at …, and there is no file
there."** Use **Choose key…** to select an existing key, or create one through
the existing System check action and choose its saved file. Building unsigned
is the third option, and it belongs on the build screen's signing choice as a
deliberate decision, not as a way out of an error.

**"The bundle at … was written, but it is not complete: N URL(s) that did not
download."** A vendor URL failed. The bundle holds everything that *did*
resolve, so this is a partial success rather than a failure. The screen names
the URLs that failed; the reason for each is in the event log, which the
event log in **Details** opens. Material failed-input reasons remain visible
in the result itself.

**Read the reason before you retry**, because there are three of them and they
have nothing in common as next steps:

- *connection refused / host unreachable* — the server is down or blocked.
  Retry later, or fetch the file another way and add it as a local `.deb`.
- *certificate signed by unknown authority* — usually a corporate proxy
  intercepting TLS, or a container with no CA certificates. Retrying will fail
  identically.
- *sha256 mismatch* — the file downloaded fine and is not the file you said it
  would be. **Retry is the wrong instinct here.** Either the digest you supplied
  is wrong, or the file at that URL changed. Check which before you build again.

**"The drive holding … filled up before the work finished."** A build writes to
two places — the output folder and the package store — and either can be the
one that filled. The message names the path that ran out. A full bundle for a
desktop target can be several gigabytes.

**"apt: could not satisfy requested packages: … (unable to locate package)."**
The target's archive does not carry that name. Go back to the picker and check
it against the catalogue; a package the archive does not have has to come in as
a vendor `.deb` URL or a local file.

**You pressed Cancel and confirmed Stop the build.** The result is a neutral
*Cancelled*, with an offer to build again. Finished downloads stay in the local
store; cancellation does not claim a usable bundle.

**On Windows: "the only debark it can find to put in that container is
…\debark.exe, which is not a Linux program."** The container backend mounts a
`debark` binary into the container and re-execs it, and on a Windows host the
one it reaches for is the `.exe`. Put a Linux build named
`bin/debark-linux-<arch>` beside the debark that Debark runs — that path is
found with no further setting — or run the build on Linux. Nothing is wrong
with the target you picked.

> **Windows readiness, and the thing that changed this round.** Until recently
> the readiness screen counted WSL 2 as a way to build, so a Windows machine
> with WSL 2 and no usable container reported a green *"This machine can build
> bundles"* over *"Nothing here stops you"* — and then every build failed. It
> no longer does. A build runs apt natively or inside a container and nothing
> else, so on Windows the container runtime is the only route, and it is gated
> by the **Linux debark for containers** row: the container mounts and
> re-runs a `debark` binary, and a `debark.exe` is not one. The WSL 2 row
> is still shown, because Docker Desktop normally runs on WSL 2 and a WSL fault
> is very often why the container row beside it failed — it simply no longer
> claims to be a build route of its own.
>
> [`windows.md`](windows.md) is the measured Windows run and is worth reading
> before you rely on any of this. Its §5 and §6 were written before the change
> above and still describe the old derivation.

### Copying to a drive, and verifying

**Not enough room.** Caught before anything is written, from the plan. Free
space on the destination, or pick another one.

**"The drive stopped responding while writing … — it looks like it was
unplugged."** The screen shows how far it got — files done of files total,
bytes written — and says what to do: plug the drive back in, wait for the
system to mount it, and export again **from the start**. Retry here means start
over, not resume. **Do not use what is on the drive now**: the incomplete
marker is still there, and the folder is not a bundle.

**"Copied, but NOT verified."** You skipped the verification pass. Nothing has
been read back off the drive, so a copy that silently went wrong would look
exactly like this one. Copy it again with verification on.

**Files that did not match.** The result lists them with the reason for each —
not on the drive, wrong length, right length but different bytes, never
written, could not be read back. *Right length, different bytes* is the one
that matters, and the one a size check alone cannot see. It usually means the
media, not the bundle: copy again to a different drive and see whether it
follows the file or stays with the stick.

**"The bundle did not verify: file size does not match the manifest."** The
bundle is not byte-for-byte the one that was signed, so it must not be
installed. The findings list under the banner names the file, the expected size
and the found size. Copy it again from the machine that built it; if a fresh
copy fails on the same file, treat the media it travelled on as suspect.

**"No debark.manifest.json in this bundle."** Check whether you chose the
folder above the bundle or whether its manifest is missing. Choose the actual
bundle folder when the path is wrong; restore a complete copy if the manifest
was deleted. Do not describe a damaged bundle as merely the wrong folder.

**"No signature verifies against a trusted key."** Do not soften this: an
unverifiable bundle must not be installed. But it is two different situations.
If you built it on this machine, the operator **public** key that was written
beside your signing key is missing — the readiness screen names the path it
looks at. If it came from somebody else, get their public key by a route other
than the media the bundle arrived on, then add it to the trusted keys.

**"This bundle carries no signature, and a bundle has to be signed for anything
to check who produced it."** If you chose to build unsigned, this is the
consequence you chose, showing up at the first check. It is not a corrupted
bundle, and — unlike the failure above it — there is no key to go and find,
because nothing signed it. Either build again with a signing key, or accept
that whoever installs it will have to pass `--allow-unsigned` and will have no
way to tell that it is the bundle you built.

---

## What Debark will not do

Not a disclaimer — the shape of the tool, and each of these has a reason.

**It does not run on the offline target.** It runs on the builder, and it never
assumes a display exists on the other side. Targets are frequently headless,
serial-console or approval-gated; over there it stays the `debark` CLI, or
plain apt. Nothing you do here changes that machine's tooling.

**It does not write bootable media and never touches a block device.** No USB
imaging, no formatting, no partitioning, no `.img` or `.iso` export, no raw
device access, and no request for administrator rights. Export copies files
into a mounted filesystem and reads them back. This was the most dangerous
component in the product and it was cut deliberately; bundle export plus a
plain file copy delivers the value.

**There is no hosted service and no account.** The catalogue comes from the
distro archive. The only hosts Debark contacts are the archives named in the
target's own sources and vendor `.deb` URLs you typed yourself. There is no
debark-operated endpoint to talk to, and there never will be.

**There is no telemetry, no crash reporting and no analytics.**
Architecturally absent rather than switched off — no such library is in the
tree, and a test in the repository fails if one appears.

**There is no update check.** The app never phones home to ask whether it is
current. Updates arrive through your distribution's patch stream like any other
package.

**There are no caps, no metering and no entitlement checks.** This is community
functionality against a published promise.

**And it decides nothing about packages.** Debark contains no dependency
resolution, no version comparison and no dependency reasoning of its own. Every
decision about what goes into a bundle is made by `debark`, which makes it by
asking the target release's own apt. The catalogue reads apt data only to show
you a list; if it is ever wrong the cost is a missing row in a picker, and you
can still add that package by name or by URL. A catalogue bug cannot produce a
wrong bundle.

---

## Where to look next

- [`../README.md`](../README.md) — what the product is, what is deliberately
  not built, and how to build it from source.
- [`dev/error-catalogue.md`](dev/error-catalogue.md) — recorded failures, each
  with the exact message, why it says what it says, and how to cause it again.
- [`dev/cli-surface.md`](dev/cli-surface.md) — the `debark` commands Debark
  runs, their exit codes and their JSON. Useful when you want to reproduce
  something at a terminal.
- [`windows.md`](windows.md) — what was run on Windows, what it produced, and
  what is deliberately not promised.
- [`packaging.md`](packaging.md) — the `.deb` itself: what is in it, what it
  depends on, which releases it was installed on, and the twenty-row check that
  proves it needs no privilege.
- `man debark-gui` — the short reference page the package installs: where the
  catalogue cache lives, which environment variables change what, and what the
  exit status means.
- `debark --help` — the CLI. Anything Debark can do is expressible as a
  command, and the build screen shows you that command before it runs it.
