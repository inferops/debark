# The error catalogue

> The capture journals, screenshots and raw logs cited in this document are
> part of the project's internal records and are not published with the
> source. The measurements they back are reproduced here in full.

## Desktop simplification: current handling, 2026-09-07

The historical rows below retain the exact failures and native rendering
evidence from their recorded second- and third-round builds. Their old screen names,
button labels and screenshots do not certify the redesigned views. The
current [operator guide](../user-guide.md) and [UX contract](ux-contract.md)
define the three-stage workflow. This addition is sourced from implementation
and named automated tests; it does not pretend the new cases were all caused
in the native application.

| Trigger | Current result and recovery | Evidence |
|---|---|---|
| Build/copy is active and a target, selection or conflicting job mutation arrives | `app.busy`; the binding rejects the mutation. The screen gives the active-job reason while viewing/navigation remain available. Target resolution rechecks the guard before committing its result. | `TestUXActiveBuildBlocksConflictingBindingsAndCapturesDraft`, `TestUXTargetResolverCannotCommitOverAStartedBuild`, `TestUXCopyBlocksBuildAndCancellationKeepsIncompleteMarker`. Native edit-blocker walk pending. |
| Package preparation fails or the user cancels | Packages shows the failure/cancelled state with explicit Retry. Reentry cannot automatically loop the operation; a ready cache takes precedence and old-generation completion cannot repaint the current target. | Package-list demo's `checkPreparationController`, executed under Node. S2 Continue reached exactly one fixture preparation call. Native failure/cancel walk pending. |
| The latest removal/Clear token expired after another mutation or a target change | `app.invalid_input`: “That selection change can no longer be undone.” Hint: “Only the most recent removal or clear can be restored. Add the packages again.” Selection stays unchanged. | `TestUXUndoInvalidatesAfterInterveningMutations`; fixture state checks. Native Undo dialog/focus walk pending. |
| Remove/Clear/Undo involves credentialed vendor URLs | Go retains original order, URL and digest internally. Selection display and copied commands remain redacted; Undo never reconstructs inputs from displayed text. | `TestUXUndoRetainsOriginalVendorInputsWithoutReturningCredentials` and audit suite. Current hostile-screen run pending. |
| Stop and close exceeds its bounded cleanup wait | `app.busy`, retryable: “Work has not stopped yet. Debark is still open.” Hint: “Wait for the running operation to finish, then try Quit again.” The app stays open and never force-closes at timeout. | `TestUXCloseTimeoutKeepsAppOpenAndAllowsRetry`; native timing walk pending. |
| A close failure remains in `LifecycleStatus.error` after jobs finish | Show the actionable close notification. A valid lifecycle projection with all running/stopping booleans false allows edits; the recoverable error alone is not an active-job lock. A missing/invalid status response is a separate unavailable-status failure and must not enable conflicting edits. | Current projection and screen logic; regression fixture/native walk pending. |
| Signing key chooser is cancelled | Keep the existing reference/choice. The chooser returns a reference, never private-key contents. Choosing unsigned remains a separate deliberate action. | Generated binding signature and build demo; native chooser walk pending. |
| Key creation succeeds | The existing readiness action returns its unchanged Result shape. Explain where the command wrote the key and ask the user to Choose key; do not infer a usable reference from success or silently use unsigned. | Current build/readiness handlers and build demo. Native keygen/chooser round trip pending. |
| Command validation fails while Command or Advanced is collapsed | Gate Build bundle with an actionable visible reason and a path to the relevant controls; preserve diagnostic code/hint/details. A closed command panel cannot hide a blocking error. | Current `PreviewCommand` handling and build demo. Native validation walk pending. |
| Existing-bundle chooser is cancelled or an old copy event arrives | Cancel returns to the opener. A new source clears old result presentation; active copy/status and source identity govern recovery, not the current draft target. | Current export route/status guards; final copy journey pending. |
| Copy verification is skipped, fails or is cancelled | Preserve unchecked/incomplete warnings and backend marker semantics. Do not turn copied bytes into a verified/signature-trusted claim. A later copy restores verification by default. | Existing exporter tests and current export implementation; final native copy/verification walk pending. |

The scoped Go race suite for `internal/app` and `internal/audit` passed for the
native-bindings packet (`0e60bfb`), including late completion, immutable draft
checkpoints, shutdown cleanup and selection security. A Windows build and the
coordinator's Linux production build passed. Those are build/test results;
they do not prove native dialog input, speech, or live offline installation.
Final Wails evidence must record the built source/binary identity and the
actual entry path for each affected case; fixture-seeded outcomes stay labelled
as fixtures. See [visual review](ui-review.md).

## Historical failure and rendering record

Every failure this application can put in front of an operator, caused on
purpose, with the exact `UIError` the backend now produces and what the screen
must do with it.

**This is a specification for the frontend, not a report.** The screens are
being redesigned; they will be built against these rows. Each one answers four
questions in order:

| What the operator did | What actually happened | The `UIError` the backend emits | What a person must see |
|---|---|---|---|

The third column is the contract. `internal/app`'s `UIError` is the only error
shape that crosses the Wails bridge (`docs/dev/binding-surface.md`), and every
row below is a real one, captured from a real run — not a plausible one written
at a desk.

---

## Provenance

Nothing here was imagined. Each row names the invocation that produced it, and
they can all be re-caused.

| | |
|---|---|
| **debark, second round** | built from the engine at `6549f6f` ("Record that the container backend has now been run, from Windows"). Two builds: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` for the container rows, and a native `windows/amd64` build for the two failures that only exist on Windows. |
| **debark, third round** | built from the engine at `5d26dac` ("Stop a slow network changing the bundle id"), eight commits past `6549f6f`, `CGO_ENABLED=0` for linux/amd64. That tree carried four files modified and not committed — `README.md`, `internal/cli/cmd_build.go`, `internal/cli/cmd_frombase.go`, `internal/cli/root.go` — which belong to unrelated work and are **help text only**; nothing on any path below reads them. |
| **A deliberately old debark** | built from `10ec7bf`, the commit before `snapshot list-bases` and `build --base` existed. Extracted with `git archive` so the engine was never touched. |
| **Driver, second round** | the application's own Go layer — `internal/app` on top of `internal/cliadapter` — through the same bound methods the frontend calls (`AppInfo`, `ListBases`, `InspectSnapshot`, `SelectTarget`, `AddPackages`, `AddURLs`, `StartBuild`, `StartVerify`, `PlanExport`, `StartExport`). Nothing below shells out to `debark` directly; that would prove debark works, which was never in doubt. |
| **Driver, third round** | **the shipping desktop application itself**, one layer further out — the real `debark-gui` binary built `-tags "desktop,production,webkit2_41"`, running on WebKit2GTK 2.52.6 under Xvfb, driven with `xdotool` and photographed with ImageMagick. Every row below that says "re-driven" was re-caused by clicking through the real screens, because a `UIError` that is correct and a screen that renders it as a bare list are the same defect to an operator. See "The rendering pass" below. |
| **Where** | Docker 29.6.2 with Linux containers. Second round: image `debian:bookworm-slim`, building `--base debian:12/minimal --arch amd64 -- apt:jq`. Third round: image `debark-shots:go126` (Ubuntu 24.04.4, `libwebkit2gtk-4.1` 2.52.6, GTK3, Xvfb, openbox, xdotool), building against a real `ubuntu 24.04 noble amd64` snapshot taken in that container, against the live Ubuntu archive (85,565-package catalogue). Plus the Windows 11 host itself. |
| **When** | 2026-09-06 and 2026-09-07. |

### The rendering pass

The second round drove every row through `internal/app` and fixed the `UIError`
half. The frontend was frozen at the time and was then rebuilt, so **what a
person actually sees was unverified for all 21 rows** until this pass. Each
row's fourth column now records what the real screen rendered, on the real
engine, and says pass or defect against what the row asks for.

**Where it got to.** The pass ran in two sittings. The first drove sections 1
and 2 — eight rows re-driven, two established as not causable through the UI
with the reason recorded. The second drove sections 3, 4 and 5. **Of the 22
rows this document now holds, 19 were driven through the real screens.** The
three that were not, each for a stated reason in its own row: §1.4 and §1.5,
which are unreachable from the screens as they are built, and §3.9, which is
Windows-only by construction and cannot be caused in a Linux container. Nothing
in sections 3, 4 or 5 was skipped for time.

**The count went from 21 to 22.** §3.3, §3.4 and §3.5 became three rows because
core `1c767f7` removed the reason they were one, and §3.10 is new — the
command-preview refusal, which is the only row here where nothing ran and the
only one whose render site is the command panel. Its own section says why that
was judged to earn a row rather than a footnote.

Four things that came out of doing it this way rather than at the bound-method
layer, and that no amount of reading would have found:

- **Three inputs can fail in one build for three unrelated reasons and the
  screen showed one sentence for all three.** §3.3-3.5 asked for the reason
  beside the input; the screen listed three bare URLs under "these URLs failed
  after their retries. A vendor URL that has moved is the usual cause", which
  was true of none of them. Fixed.
- **Every failure offered exactly one action, "Change the options".** Four rows
  name a remedy that lives on another screen — the readiness screen's container
  runtime (§2.4), its signing key (§3.2), the picker (§3.8), the vendor URL that
  failed (§3.3-3.5) — and none of them had a route there. Fixed.
- **A vendor URL's password is still on this screen, with a Copy button under
  it**, in the "Could not be downloaded" list, after every neighbouring surface
  stopped showing it — and the same disagreement about redaction silently
  breaks the join that puts the reason beside the input. See §3.3-3.5. Reported;
  the fix is one call in `internal/app`.
- **`verify` on a folder whose manifest was deleted told the operator they had
  picked the wrong folder.** They had not: the folder was the bundle. Core
  `5837d82` split that case from the genuine wrong-folder one and this
  document's rule had not followed. See §4.2. Fixed.

And four methodological notes, each of which cost time:

- **A screenshot of a window whose lower half is off-screen comes back blank**,
  not clipped. Two "white screens" in the first sitting were the capture, not
  the app. Move the window to 0,0 and capture the root, or confirm a blank
  against a second capture method before recording it as a defect. The window
  openbox maps is 40 px down, so a 1400x900 window on a 1400x900 screen is
  already clipped: size it 1400x840.
- **A native GTK dialog is a separate X window** and `import -window <app>`
  photographs it as a solid black rectangle. Capture the root instead. It also
  ignores `xdotool key --window`, which uses `XSendEvent`; a real pointer click
  followed by XTEST keys reaches it.
- **The wheel scrolls whatever is under the pointer**, and on the export screen
  that is usually the drive table, which has its own scroll port. Put the
  pointer in the left margin, or press Tab and then Page Down.
- **A copy that finishes before the drive is pulled is not a test.** §5.1's
  first attempt flipped the volume 1.6 s into a 2 s copy and the export
  *succeeded*: the writes were in page cache and the verification read them
  back from cache. Interrupt early, inside a large file.

### This catalogue postdates eight core commits, and that matters

`docs/dev/cli-surface.md` was verified against debark `94612f5`/`bf56866`.
**Eight** commits have landed in the core repository since — six during the
second round's pass and two more during the third round's — and several change
error behaviour so fundamentally that rows were driven twice, once before and
once after. Only the "after" is recorded here.

- **`3ed80bd`, "Make a failing command say why on stderr, and name what
  failed".** `silentError` is gone. `build`, `verify`, `install` and `fetch`
  used to print a complete `--json` document to stdout and **zero bytes** to
  stderr; they now print a line naming what failed. **Caveat C1 and caveat C7
  in `cli-surface.md` are no longer true at core HEAD.** That document is
  owned by another package and the correction has been reported rather than
  made here.
- **`2d6d5c7`, "Stop telling an operator who picked the wrong file that their
  media was tampered with".** `snapshot inspect` on a structurally wrong file
  now exits **1 (`usage`)** rather than 4 (`verification`). Caveat C8 is
  fixed. The boundary is structural, not a general softening — see the two
  snapshot rows below, which are now genuinely different failures.
- `a0b61c2` added `--self-binary`, `DEBARK_SELF_BINARY`, the `self_binary`
  config key and `bin/debark-linux-<arch>` discovery. The ELF-mismatch
  failure is still reachable — it is what happens on a fresh Windows install
  where none of those is set — but its hint is no longer a Go struct field.
- `4d81294`, `09d7821` and `6549f6f` do not change any row here.

Since then, from the core-engine work in the third round, and every one of these
was re-caused against the new binary before it was recorded:

- **`1c767f7`, "Say why a download failed, not just that one did".**
  `BuildResult` gains `fetch_failures[]` of `{input, reason, detail}` beside the
  untouched `fetch_failed[]` — a parallel field, not a widened one, because
  `docs/formats.md` §0 fixes a published schema version as immutable. Twelve
  reasons, a closed set with `other` in it, tagged at each return site rather
  than matched out of prose. **§3.3, §3.4 and §3.5 are three rows now** instead
  of one, and the reason they were one is gone.
- **`5837d82`, "Stop telling an operator who picked the wrong folder that their
  bundle failed verification".** `verify` on a directory that is not a bundle
  exits **1 (`usage`)** with **zero bytes on stdout**. A real bundle whose
  manifest was deleted still exits 4, because that genuinely is tampering. §4.1
  and §4.2 are now two different failures and say so.
- **`d5f31cb`, "Say on the wire when apt could not fetch a single index".**
  `apt.update` carries `sources_fetched`/`sources_failed`. It deliberately does
  not fail the run: apt is the oracle (ADR-001). Re-driven both ways this pass
  — `{"sources_failed":4,"sources_fetched":0}` with `--network none` against
  `{"sources_failed":0,"sources_fetched":4}` without — and the build screen now
  reads it. See §3.1.
- **`9c77bbe`** fixed the `debark key generate` hint named in §3.2.
- `034f709`, `92ebdc2`, `bf51914` and `5d26dac` do not change a row here.
  `5d26dac` is worth knowing while driving one, though: `progressWriter` fired
  on a 250 ms wall clock, so a build with a vendor URL emitted 2 events when
  fast and 9 when slow and produced two bundle ids for the same bytes. **Do not
  assert on event counts.**

Where a row changed because of one of these, it says so.

### Re-causing any of this

Every row's "how it was caused" cell is a complete recipe. The only piece not
written down is the driver, which was a throwaway `main` compiled through
`go build -overlay` from outside the tree, so that nothing landed in a
repository other packages were committing from. It did no more than call the
bound method and print the `UIError` as JSON; any equivalent harness will do,
and `hack/e2e` is the shipped one to model it on.

---

## How to read the fourth column

"What a person must see" is deliberately more than "render `message` and
`hint`". Three things recur, and a screen that does only the obvious thing will
be wrong in the same way three times:

1. **`message` and `hint` are the whole banner.** `message` is one sentence
   saying what is wrong; `hint` is what to do next. Neither is ever empty —
   that is enforced structurally in `classify`, not by review. Render both.
   Never render `exit_code` as the headline; it is supporting evidence and
   `details` is where the raw bytes live.
2. **The `UIError` is often not the whole story, and the row says where the
   rest is.** A failed build's `UIError` cannot name the reason a particular
   URL failed, because the process that produced it did not put that anywhere
   the adapter could see. It is in the event log. Rows that need this say
   **"reach for"** and name the field.
3. **Some failures are not errors.** A cancelled build produces no `UIError` at
   all, on purpose. A screen that shows an error banner because `ok` is false
   would be reporting the operator's own decision back to them as a fault.

---

# The rows

## 1. Choosing a debark — the About panel and first run

### 1.1 debark is not installed

| | |
|---|---|
| **Caused by** | `AppInfo()` with no `debark` on `PATH` and none beside the executable. |
| **Happens** | Discovery is lazy and never cached on failure, so this is retried on the next call — installing debark and pressing Retry works without a restart. |

```json
{ "code": "cli.missing",
  "message": "the debark command-line tool was not found (searched PATH and C:\\…\\errors)",
  "hint": "Install the debark package, or set the full path to the binary in settings. The path this app tried is in the command below.",
  "command": ["debark"],
  "exit_class": "environment", "exit_code": 127, "retryable": true }
```

**On screen.** This is the one failure that blocks every screen, so it belongs
on the readiness screen as a blocking row, not in a banner over an empty
picker. `retryable` is true and means it: offer Retry, and make it re-probe
rather than restart. Show `message` verbatim — it names both places that were
searched, which is the first question anyone asks.

**Re-driven through the UI: PASS.** The readiness screen carries a **Blocking**
row for the debark check, a summary banner naming it as the one thing that
stops a build, and a **Re-check** action that re-probes without restarting. The
global banner over it shows `message` verbatim, including "(searched PATH and
/tmp/nodf)", and its "What failed" drawer holds the argv, the class and the
code. The row's own "Details" drawer repeats both searched places. Nothing is a
toast; nothing shows an exit code as the headline.

### 1.2 The configured path is not debark at all

| | |
|---|---|
| **Caused by** | `AppInfo()` with the debark path pointing at `git.exe`, then at `where.exe`. Both real, both on the Windows host. |
| **Happens** | `Probe` runs `version --json --no-color` first. git exits **129** and complains about an unknown option; where.exe exits **1** — inside debark's own frozen table — and prints an `INFO:` line. |

Before, git's own text became the message and git's usage line became the hint.
Now:

```json
{ "code": "cli.missing",
  "message": "the program this application is running as debark does not answer like debark: it exited with status 129 and printed \"error: unknown option `json'\"",
  "hint": "Point the debark path in settings at the debark binary itself, or clear it so that PATH is searched instead. The command that was run is below.",
  "command": ["debark", "version", "--json", "--no-color"],
  "exit_class": "environment", "exit_code": 129, "retryable": true }
```

**On screen.** Same place as 1.1 — a blocking readiness row — and the action is
"choose the debark binary", a file picker, not a plain Retry.

**Re-driven through the UI: DEFECT, and the row was partly wrong.** Caused by
putting a symlink to `/usr/bin/git` named `debark` on `PATH`, which
reproduces the exact case this row records. Two findings:

1. The readiness row is **Limited**, not **Blocking**, and the summary banner
   above it still says *"This machine can build bundles."* It cannot: every
   build will run git. That classification is `internal/readiness`'s, not the
   screen's, and it is reported rather than fixed here.
2. **The action this row asks for does not exist and cannot.** There is no
   settings surface for the debark path anywhere in the application:
   `cliadapter.Options.BinaryPath` is never assigned (`main.go` passes an empty
   `Options{}`), so "choose the debark binary" would need a bound method and
   somewhere to keep the answer. The row is asking for a feature, not a
   rendering.

What the screen does render is better than the row expected in one respect: the
readiness row names the offending file — *"A binary at /tmp/fake/debark did
not report a debark version"* — which is the one fact the `UIError` still
lacks, and offers "Copy command", "Show the version" and "Re-check".

**Corrected, therefore:** what this row must put on screen is a readiness row
that **blocks**, names the file that answered wrongly, and offers a way to run
it and see for yourself. A file picker for the debark path is a product
decision, not an error rendering, and belongs in the backlog rather than
here.

**Defect for `internal/cliadapter/invoke.go`, not fixed here.** `command`
says `debark`, because `invokeArgs` deliberately puts `ProgramName` in
`argv[0]` rather than the resolved path. That is right for a command an
operator might paste, and wrong for exactly this failure: the one fact the
person needs is *which file* was run, and it appears nowhere in the `UIError`.
`Probe`'s other identity check (exit 0, wrong `name` field) does name the path,
so the two halves of the same check disagree. Suggested: pass the located
binary into the failure so the summary can name it.

### 1.3 The debark found is too old — no `snapshot list-bases`

| | |
|---|---|
| **Caused by** | `ListBases("amd64")` against a debark built from `10ec7bf`. |
| **Happens** | Cobra runs the *parent* command for an unknown subcommand (`cli-surface.md` C6), so `--arch` is rejected by the `snapshot` group. stderr is `debark: unknown flag: --arch` followed by the **snapshot group's** usage block, which is the tell. |

```json
{ "code": "cli.failed",
  "message": "this debark is older than this application: it has no `snapshot list-bases`, so it cannot offer stock base targets",
  "hint": "Choose a snapshot file taken from the target machine instead — that works with every version — or install a newer debark. The About panel shows which one this application is running.",
  "exit_class": "usage", "exit_code": 1, "retryable": false }
```

Before: *"this debark does not accept the option `--arch`"* — true, and about
a flag the operator never typed, for a feature that is wholly absent.

**On screen.** The target screen must **degrade, not fail**: hide the stock-base
half and leave "choose a snapshot file" working. This message belongs where the
base list would have been, not in a modal.

**Re-driven through the UI: PASS.** Caused with a debark built from
`10ec7bf`. The target screen keeps the "A snapshot file" option live and puts a
`.df-state--error` exactly where the base list would have been, with `message`
as its title and `hint` as its text, two actions — "Try again" and "Use a
snapshot instead" — a "What failed" drawer, and a closing line saying a
snapshot needs no list at all. No modal. One nit, not a defect: `retryable` is
false and "Try again" is the *primary* action, while "Use a snapshot instead"
is the one that works.

### 1.4 The same binary, called without an architecture

| | |
|---|---|
| **Caused by** | `ListBases("")`, same old binary. |
| **Happens** | With no `--arch` there is no flag to reject, so cobra prints the group's help and **exits 0**. The adapter then cannot decode a `debark.baselist/v1` document. |

```json
{ "code": "cli.failed",
  "message": "debark printed something this application could not read as JSON (it began \"Capture the state of an offline target (everything its own apt would…\")",
  "hint": "This is nearly always a debark older than this application: one with no `snapshot list-bases` prints the snapshot group's help and exits 0, so it looks like success. Choose a snapshot file as the target instead, or install a newer debark.",
  "exit_class": "success", "exit_code": 0, "retryable": false }
```

Before, the hint was *"Run the command shown below in a terminal … which is a
bug worth reporting"* — it sent the operator to file a bug report about a
version mismatch.

**On screen.** Identical treatment to 1.3.

**Not caused through the UI, and it cannot be.** `ListBases("")` is unreachable
from the target screen: `App.SupportedArchitectures` returns
`{arches: ["amd64","arm64"], default: "amd64"}` even when its probe fails, so
`S.arch` is never empty and `loadBases` never sends `''`. The row's `UIError`
is real and was driven at the bound-method layer in the second round; its
rendering is the same code path as 1.3 (`res.error` → `S.basesError` → the
base panel's error state), which was driven and passes. Recorded as unreachable rather than
as untested.

### 1.5 The same binary, asked to build from a stock base

| | |
|---|---|
| **Caused by** | `StartBuild` with a base target, same old binary. |

```json
{ "code": "cli.failed",
  "message": "this debark is too old to build against a stock base OS: it can only build from a snapshot",
  "hint": "Choose a snapshot file taken from the target machine, or install a newer debark. The About panel shows which one this application is running.",
  "exit_class": "usage", "exit_code": 1, "retryable": false }
```

**On screen.** The build screen's error state, with a route back to the target
screen. `retryable` is false: pressing Retry cannot help.

**Not caused through the UI.** Reaching it needs a base target *selected* and
then a debark too old to build one, and the target screen will not let a base
be selected against that binary — 1.3 fires first and hides the list. It is
reachable by swapping the binary under a running app between the two calls,
which is a real situation (a package upgrade mid-session) but not one this pass
had budget to stage. The rendering is the build screen's `failed` outcome,
driven for 2.4, 3.2 and 3.8, which now carries the route this row asks for.

**Gap, reported not fixed.** `AppInfo()` against this binary returns **ok**,
with version `dev` and no warning at all. `cliadapter.Probe` knows the truth —
`Capabilities.Bases` is false — and nothing on the bound surface exposes it, so
the operator meets the problem three screens later. Suggested for
`internal/app` (W0-E): carry `Capabilities` into `AppInfo` and let the
readiness screen say "this debark cannot offer stock base targets" up front.

---

## 2. Choosing a target — the snapshot file picker

A file picker makes choosing the wrong file *likely*, so these four are not
edge cases.

### 2.1 A file that is not a snapshot

| | |
|---|---|
| **Caused by** | `InspectSnapshot` on 200 KB of `/dev/urandom` named `.tar.zst`; on a bare `snapshot.json`; and on a directory. Three separate runs, three separate messages. |
| **Happens** | Exit **1 (`usage`)** since core `2d6d5c7`. Before that it was exit 4 (`verification`) — the class that means "signature, digest or repository metadata mismatch" — which told someone who picked the wrong file that their media had been tampered with. |

```json
{ "code": "cli.failed",
  "message": "snapshot: /work/garbage.tar.zst is not a debark snapshot: it does not begin with a zstd frame header",
  "hint": "Pick the .tar.zst file that `debark snapshot create` wrote on the target machine. If you do not have one, choose a stock base OS on the target screen instead — it assumes a clean install of that release rather than describing a real machine.",
  "exit_class": "usage", "exit_code": 1, "retryable": false }
```

The other two differ only after the colon: *"it is a directory with no
snapshot.json in it"*, and the same frame-header sentence for a bare
`snapshot.json`. debark's own summary is kept because it says which of the
three it was; only its hint is replaced, because that hint named two CLI
commands, one of which (`snapshot from-base`) this application never runs and
which duplicates the target screen's own base picker.

**On screen.** Inline, next to the file field, with the picker still open —
this is a correction, not a failure of the run. Offer "choose another file" and
a link to the stock-base list.

**Re-driven through the UI: PASS, and the row overstated one thing.** Driven
with 200 KB of `/dev/urandom` named `.tar.zst` and with a bare `snapshot.json`.
Both render `message` and `hint` verbatim in the target screen's own panel,
directly under the file field, which still shows the chosen path and a "Choose
a different file…" button. Below the message: **"Choose a different file…"**
(primary), **"Read this file again"**, **"Use a stock base instead"**, a "What
failed" drawer, a paragraph explaining what a snapshot is, and the `debark
snapshot create --out target.tar.zst` command with a copy button. That is more
than the row asked for.

**"with the picker still open" is not achievable and should not be asked for.**
`ChooseSnapshotFile` opens the *native* GTK file chooser, which is modal and
closes on selection; nothing in this application can hold it open. Inline in
the target screen beside the file field, with a one-click way back to the
chooser, is the same intent and is what the redesign does. Corrected.

The third variant — a **directory** — could not be caused through the UI at
all: a GTK "open file" chooser will not return one. It was driven at the
bound-method layer in the second round and differs only after the colon.

### 2.2 A snapshot cut short by a copy that did not finish

| | |
|---|---|
| **Caused by** | `InspectSnapshot` on a real snapshot truncated to its first 4 KiB. |
| **Happens** | Exit **4 (`verification`)** — and this one is correct, because it is a *content* failure, not a structural one. The file really does start like a snapshot. |

```json
{ "code": "cli.failed",
  "message": "that snapshot file is incomplete: it ends part-way through",
  "hint": "The copy did not finish. Copy the .tar.zst again from the machine that made it, and let the copy complete before ejecting the drive — a snapshot cut short looks exactly like this.",
  "exit_class": "verification", "exit_code": 4, "retryable": false }
```

Before, this fell through to the `verification` class floor and read: *"Do not
install a bundle that fails verification. Obtain a fresh copy from the machine
that built it, and check that the key you trust is the one it was signed
with."* There is no bundle and no signature anywhere in this story.

**This is the structural/content boundary the redesign must respect.** 2.1 and
2.2 are different failures with different next steps — "you picked the wrong
file" versus "the file you picked is damaged" — and after `2d6d5c7` the exit
code tells them apart.

**Re-driven through the UI: PASS.** Driven on a real snapshot truncated to its
first 4 KiB. The two render as different sentences in the same place, which is
the boundary holding. Their *actions* are identical, and "Use a stock base
instead" is a slightly odd offer for a damaged file — noted, not a defect. `TestClassifySnapshotFailuresNeverMentionBundles` stops
the bundle-shaped class floor reaching this command again, including for
wordings nobody has written yet.

### 2.3 A snapshot file that is not there any more

| | |
|---|---|
| **Caused by** | `InspectSnapshot` on a path that does not exist. Driven on both platforms — the message differs (`stat …: no such file or directory` on Linux, `GetFileAttributesEx …: The system cannot find the file specified` on Windows) and the rendering must not. |

```json
{ "code": "cli.failed",
  "message": "there is no snapshot file at /work/nowhere.tar.zst",
  "hint": "Choose the file again. If it was on a removable drive, plug the drive back in and wait for it to appear before picking it.",
  "exit_class": "usage", "exit_code": 1, "retryable": false }
```

On Linux this previously produced the **usage class floor** — *"This is usually
a mismatch between this app and the debark it is driving. Check the debark
version in settings, and report the command shown below."* — because the
existing missing-file pattern wanted `open <path>: no such file` and the real
message is `open <path>: stat <path>: no such file`. Someone who picked a file
from an unplugged drive was told to check for version skew and file a report.

**On screen.** Same as 2.1: inline, picker stays open.

**Re-driven through the UI: PASS.** Caused by pointing the chooser at a
dangling symlink, which is what a file on an unplugged drive looks like to
`stat`. Renders *"there is no snapshot file at /fix/gone.tar.zst"* with the
hint verbatim, and "Read this file again" is exactly the right action here:
plug the drive back in and press it.

### 2.4 A snapshot for a different release than this machine

| | |
|---|---|
| **Caused by** | `snapshot create` inside `ubuntu:24.04`, then `StartBuild` against it inside `debian:bookworm-slim`, which has no container runtime. |
| **Happens** | The local backend refuses because the host distro is not the target's, and the container backend refuses because there is none. |

```json
{ "code": "cli.failed",
  "message": "neither way of resolving packages for that target works on this machine: apt here was refused (host distro \"debian\" does not match target distro \"ubuntu\"), and the container backend was refused (no container runtime found (tried docker, podman))",
  "hint": "Either install a container runtime and start it — the readiness screen checks for one and says how — or pick a target whose release matches this machine's, which lets apt resolve here directly with no container at all.",
  "exit_class": "environment", "exit_code": 2, "retryable": true }
```

Before, the message named only the container half — *"no container runtime is
available, and the local backend cannot resolve for a different release"* — and
the hint was debark's own `sudo apt-get install docker.io`. Both halves
matter: an operator told only to install Docker does not learn that a matching
target would need no container at all.

**On screen.** The build screen's error state, with two actions: "open
readiness" (which has the container-runtime row and its remedy) and "change
target".

**Re-driven through the UI: DEFECT, now FIXED.** Caused the mirror image of the
second round's run — an `ubuntu` host container with no runtime, targeting
`debian:12/minimal` — which takes the same path and produces the same sentence
with the two distros swapped. The banner renders `message` and `hint` verbatim,
with the class and exit code as supporting text below it and a "What failed,
verbatim" drawer holding the argv and the full stderr. It survives navigating
away and back.

**What was missing: both actions.** The only action was "Change the options".
The hint says *"the readiness screen checks for one and says how"*, and there
was no way to get there. `build.js` now routes on the exit class — an
`environment` failure offers **"Open readiness"** — and the target screen is one
step further on from there.

**A second defect this row found, and it is worse than the missing route.** The
build screen went on showing this failure after the operator did exactly what
the hint said. Change the target to one whose release matches, come back, and
the screen still asserted — in red — that a target no longer selected could not
be resolved; the same after adding or removing a package. `build.js` now
records the target and selection a run was started against and returns to the
form when they no longer match.

---

## 3. Building

### 3.1 No network at all

| | |
|---|---|
| **Caused by** | `StartBuild` in a container run with `--network none`. **Reproduced a second time** with `http_proxy`/`https_proxy` pointed at an unroutable address, byte-identically. |
| **Happens** | apt's indexes cannot be read, so even the base definition's own seed packages look absent. Exit **5 (`resolution`)**. |

```json
{ "code": "cli.failed",
  "message": "the target's package indexes could not be read, so even the base system's own packages look missing",
  "hint": "This is almost always the network rather than the packages: the seeds that failed belong to the base definition, not to anything you chose. Check that this machine can reach the archive named in the target's sources, and check the proxy settings if it uses one, then build again.",
  "exit_class": "resolution", "exit_code": 5, "retryable": true }
```

Before: debark's own line, *"apt: base seeds do not resolve against this
release's archive: apt (unable to locate package); init (unable to locate
package)"*, over the resolution class floor, *"Check the package names against
the catalogue for this target."* The operator had not typed `apt` or `init`.
This was the single most misleading rendering found, and it is what a laptop on
a captive-portal wifi produces.

**On screen.** The build screen's error state. `retryable` is true; Retry is the
right primary action. The details drawer will show `apt.update` — see the core
gap below, which is why the drawer alone would not have helped.

**Core gap.** In the `--network none` run the `apt.update` event carried
`{"entries": 12, "failed": false}`. Nothing on the wire said the network was
down; the only signal was a resolution failure naming packages the operator
never chose. Suggested: have `apt.update` report a genuine failure when no
index was fetched, or emit a distinguishable event when every source is
unreachable.

**Re-driven through the UI: PASS.** Caused in a container run with `--network
none`, reaching the picker at all because the catalogue was already cached from
an earlier online run — which is itself the honest shape of this failure, since
an operator meets it on a machine that worked yesterday. The build screen
renders `message` as the banner title and `hint` as its text, both verbatim,
with `debark class resolution (exit 5): resolution failed: apt could not
satisfy the request` as supporting text below, a "What failed" drawer, and
three actions: **Build again** (`retryable` is true and it is the right
instinct here), **Back to packages** and **Change the options**.

**The core gap this row named is closed, and the UI half of it was still
open.** The `apt.update` event in that run read

```json
{"entries":16,"failed":false,"sources_failed":4,"sources_fetched":0}
```

against `{"entries":19,"failed":false,"sources_failed":0,"sources_fetched":4}`
from the same build with a network — so core `d5f31cb` does put it on the wire.
The screen was not reading it: `build.js` warned only on `failed === true`,
which ADR-001 means is essentially never set, so that branch had never fired in
its life. It now raises **"No package index could be fetched"** when
`sources_fetched` is 0 and something failed, and **"Some package indexes could
not be refreshed"** when only some did. Both read integers off the event;
neither parses prose.

**The second case matters more than this row does.** A build whose indexes are
stale does not fail — apt is the oracle — so it succeeds, every version in the
bundle is whatever was last cached, and until now nothing on the screen said
so. This row is the loud version of a failure whose quiet version is worse.

### 3.2 No signing key

| | |
|---|---|
| **Caused by** | `StartBuild` with the signing key set to a path with no file. |

```json
{ "code": "cli.failed",
  "message": "the build was told to sign with the key at /work/absent.key, and there is no file there",
  "hint": "Create an operator signing key — the readiness screen has an action that does it — or point the signing key setting at a .key file that exists. Building without a signature is the other option, and it is a deliberate choice rather than a default.",
  "exit_class": "usage", "exit_code": 1, "retryable": false }
```

Before: *"/work/absent.key does not exist"* with *"the file may have been moved
or renamed, or the volume it is on may not be mounted"* — true of any file, and
silent about this one being the signing key that the application itself can
create.

**On screen.** The build screen, with the readiness screen's `signing-key`
remedy offered as an action. Do not offer "build unsigned" as a one-click
escape: it is a decision, and it belongs on the build options, deliberately.

**Core-repo defect.** debark's own hint here is *"generate one with `debark
key generate`"*. There is no `debark key generate`; the command is `debark
keygen --out PATH`. `core/sign/ed25519.go:43` at `6549f6f`. Anyone who follows
that hint at a terminal gets an unknown-command error.

**Re-driven through the UI: PASS.** Caused by pointing the build screen's
signing key at `/work/absent.key`. `message` and `hint` render verbatim as the
banner, with the class and exit code below and a "What failed" drawer holding
the argv and the whole stderr. The action bar offers **Open readiness** — the
remedy the hint names by name, and the route the previous pass added — beside
"Change the options". "Build again" is correctly absent: `retryable` is false.
Nothing anywhere offers "build unsigned" as a one-click escape; that choice
stays on the build options, where the row asks for it.

**The core-repo defect this row reported is fixed.** debark's stderr in this
run reads *"generate one with `debark keygen --out /work/absent.key`"*. Core
`9c77bbe`.

**A defect this row found that is not about signing.** The metrics strip read
**"SELECTIONS RESOLVED: 147"** on a run whose operator had chosen two items and
whose only `apt.resolve` event was `{"packages":147,"seeds":1}` — the base's
own closure, before the build had looked at anything the operator asked for.
Two different numbers were sharing one label. `build.js` now says **"Base
packages resolved"** when the event carried `packages` and keeps "Selections
resolved" for the event that carried `selections`.

### 3.3 A vendor URL that cannot be reached

### 3.4 A digest that does not match

### 3.5 A certificate nothing trusts

**These were one row, and they are three now.** They shared a row because they
shared everything a caller could see: all three exit 3 (`incomplete`), all
three write a stderr line differing only in the URL, and all three put the URL
in `BuildResult.FetchFailed` with **no reason attached**. Core `1c767f7` ended
that -- `fetch_failures[]` of `{input, reason, detail}`, twelve reasons, a
closed set with `other` in it, tagged at each of 34 return sites and never
matched out of prose -- so an unreachable host, a SHA-256 that did not match
and a certificate this machine does not trust are now three different answers
to "what do I do next", and a document that gave them one answer was giving two
operators the wrong one.

They keep one shared `UIError` and one shared screen, and that is not an
oversight: the failure the *build* had is the same in all three -- the bundle
was written and it is short -- and what differs is per input, which is where
the screen now puts it. The three sub-sections below say what each one is; the
shared contract under them says what the screen must do with all three at once,
because one build can produce all three.

**§3.3, an unreachable host.** A closed port: `http://127.0.0.1:9/...`. The
reason is `unreachable`; the detail is `dial tcp 127.0.0.1:9: connect:
connection refused`. Act on the network, the proxy or the address. Retrying is
reasonable.

**§3.4, a digest that does not match.** A real `.deb`, served intact, with a
deliberately wrong `--digest`. The reason is `digest-mismatch`; the detail
names both hashes. **This is the one reason where retrying is the wrong
instinct**: the bytes arrived perfectly and either the digest is wrong or the
file is not the one that was expected, and both need a person. It is also the
only one of the three that is a *verification* failure wearing an
`incomplete` build's clothes.

**§3.5, a certificate nothing trusts.** An `https://` download whose server
presents a certificate with no trust anchor on this machine. The reason is
`tls-untrusted`; the detail is `x509: certificate signed by unknown
authority`. The fix is on **this** machine -- a corporate root that is not
installed, or a proxy intercepting TLS -- not at the far end, and core stops
retrying for exactly that reason.

```json
{ "code": "cli.failed",
  "message": "the bundle at /work/bundle was written, but it is not complete: 1 URL that did not download: http://127.0.0.1:9/vendor-tool_1.0_amd64.deb",
  "hint": "The bundle holds everything that did resolve. Open the details drawer: the log names the reason for each input that is missing — an unreachable host, a certificate this machine does not trust, a SHA-256 that did not match the digest you supplied, or a dependency the target's archive does not carry. Correct the input, or add the file from disk instead, and build again; whatever already downloaded is reused.",
  "exit_class": "incomplete", "exit_code": 3, "retryable": true }
```

Before core `3ed80bd` this was silent: exit 3, zero bytes on stderr, and the
generic *"the bundle was written but it is not complete"*. debark now names
the URL. Its hint, however, tells a person at a window to pass `--local-dir`,
which is why this row overrides it.

**The reason lives in exactly one place.** For each of the three, the only text
that distinguishes them is the `error` attr of a **warn-level `input.external`
event**:

```
connection refused  fetch: http://127.0.0.1:9/…: request failed: Get "…": dial tcp 127.0.0.1:9: connect: connection refused
digest mismatch     fetch: http://deb.debian.org/…/hello_2.10-3_amd64.deb: sha256 mismatch: expected aaaa…, got 2e6e2f1a…
untrusted TLS       fetch: https://deb.debian.org/…/hello_2.10-3_amd64.deb: request failed: Get "…": tls: failed to verify certificate: x509: certificate signed by unknown authority
```

**On screen — this is the most demanding row in the catalogue.**

1. Render `message` and `hint` as the banner.
2. **Reach for `BuildStatus.Summary.FetchFailed`** and list every URL that
   failed, next to the banner, not buried in a drawer. It is the list the
   operator has to act on.
3. **Reach into the event log** (`BuildLog`) for every event with
   `level == "warn"` and `type == "input.external"`, and show its
   `attrs.error` beside the URL it names in `attrs.url`. Without this the
   operator cannot tell "the server is down" from "you typed the wrong
   SHA-256" from "your company proxy is intercepting TLS", and the three
   have nothing in common as next steps.
4. Also reach for `Summary.Unresolved` (an external `.deb` needing something
   the target cannot supply) and `Summary.Warnings`. A plaintext-HTTP warning
   fires here too and is worth showing.
5. `retryable` is true — but a digest mismatch is the one case where Retry is
   the wrong instinct, and the hint says so. Do not label the button "Retry"
   alone; "Build again" next to the list is honest.

**The core gap this row reported is fixed, and the fix is not wired all the
way through.** `BuildResult` now carries `fetch_failures[]` beside the
untouched `fetch_failed[]`, one entry per entry, same order, with a
machine-readable `reason` and a redacted `detail` (core `1c767f7`). But
`internal/app`'s `BuildSummary` projects only `fetch_failed[]`, so the
frontend still cannot see the classified reason and still has to correlate
two streams by URL. **Suggested, and it is small: project `fetch_failures[]`
into `BuildSummary` and render the reason from the closed set rather than from
the event's English.** Until that lands, the screen reads the event -- which
works, and see the defect below for where it stops working.

**A note on the TLS rule.** `errors.go` carries an `environment/tls-untrusted`
row with a hint about installing a corporate root certificate. It is marked in
the code as **not driven**, because on the build path the x509 text never
reaches stderr — it is only ever in that event. The row exists for the paths
where the same failure *is* classified `environment` and does print (`snapshot
from-base`, and any future fetching command); the build path is covered by
naming the cause in the hint above.

**Re-driven through the UI: PASS on four of the five points, in one build, and
the fifth is where the row is now wrong.** Driven in the shape that made these
one row: **three inputs, three reasons, one run.** A closed port at
`127.0.0.1:9`; a real `hello_2.10-3build1_amd64.deb` served over plain HTTP
from a throwaway server with a deliberately wrong `--digest`; the same file
over `https://` from a throwaway server with a self-signed certificate. All
three servers inside the container, nothing outside it touched.

The screen listed each input with its own reason underneath it:

```
http://127.0.0.1:9/vendor-tool_1.0_amd64.deb
    fetch: ...: request failed: Get "...": dial tcp 127.0.0.1:9: connect: connection refused
http://127.0.0.1:9391/hello_2.10-3build1_amd64.deb
    fetch: ...: sha256 mismatch: expected aaaa..., got e68cf4365b7aa9c4e2af4af6eee1710d6f967059b7b4af62786e8870d7366333
https://127.0.0.1:9392/hello_2.10-3build1_amd64.deb
    fetch: ...: request failed: Get "...": tls: failed to verify certificate: x509: certificate signed by unknown authority
```

under a "Could not be downloaded (3)" heading beside the banner, with a "Copy
this list" action and the sentence *"Correct the input, or add the file from
disk instead, and build again — whatever already downloaded is kept in the
local store and is not fetched twice."* Point 2 and point 3, which are the
whole reason this row existed, hold. Point 4 holds: two plaintext-HTTP warnings
and two retry warnings are in "Warnings, as they arrive" below. Point 5 holds:
the action is **"Build again"**, never "Retry", next to the list.

**Point 1 is wrong now, and the redesign is right.** The banner is not
`message` and `hint`. It is the screen's own sentence about what an incomplete
bundle *is* — *"A bundle exists at the path below and it can be copied. It is
missing the items listed here, and anything that depends on them will fail to
install on the target."* — and the facts `message` carries are rendered as
structure instead: the bundle path in its own panel with a copy button, the
failing inputs in the reasoned list above it. Rendering `message` as the
headline would put the same three URLs in a sentence directly above a list of
the same three URLs. And the `hint` says *"Open the details drawer: the log
names the reason for each input that is missing"* — advice that was true when
it was written and is now an instruction to go and look elsewhere for something
already on the screen.

**Corrected, therefore.** For the `incomplete` outcome the banner is the
screen's own, and what this row requires is that **the bundle path, every
failing input, the reason for each, and the "build again, nothing is fetched
twice" advice are all on the screen without opening anything.** They are. The
stale drawer-pointing sentence lives in `build/incomplete`'s hint in
`internal/cliadapter/errors.go`; it is not rendered anywhere, so it is recorded
here rather than quietly deleted.

**DEFECT, and it is a credential leak with a button on it. Reported, not fixed:
the fix is not in this package files.** Driven with
`https://ops:s3cr3t@127.0.0.1:9392/hello_2.10-3build1_amd64.deb`:

- The **"Could not be downloaded (1)"** list renders the URL **with its
  password in plain text**, and offers **"Copy this list"** directly beneath
  it. Every other surface on this screen stopped doing that — GUI `d30b316`
  redacted the command preview, `0853f19` redacted `UIError.Command` — and this
  one panel, one heading further down, still does it and puts it on a
  clipboard.
- **The reason vanished.** The event's `attrs.url` is `https://REDACTED@...`,
  because debark redacts URLs in its own event stream, while
  `Summary.FetchFailed` is the operator's literal string, because core
  deliberately does not redact it there. `build.js` joins the two by string
  equality, so for exactly the URLs carrying a credential or a query string the
  join misses and the input is listed with no reason at all. The reason is
  still on screen — in "Warnings, as they arrive", against the redacted URL —
  so this degrades rather than disappearing; but requirement 3 stops holding
  for the one URL shape that most needs it, because a presigned link is how a
  vendor ships a private `.deb`.

Both are one root cause — two streams disagreeing about redaction, joined by
string equality — and one change fixes both, **in `internal/app`, which this
package does not own**: redact `FetchFailed` in the `BuildSummary` projection
(`bindings.go`, at the `clampStrings(r.FetchFailed, ...)` call) with the same
`fetch.RedactURL` the event stream and the command preview already use. That
makes the join succeed *and* takes the credential off the clipboard, and it
needs no second redactor in JavaScript — which is the thing
`internal/cliadapter/redact.go` argues at length against. Core's reason for
leaving `FetchFailed` unredacted, that the operator typed the string
themselves, is sound for a result document and does not survive contact with a
Copy button.

### 3.6 The disk fills up

| | |
|---|---|
| **Caused by** | `StartBuild` with the output folder on a 256 KiB tmpfs (`docker run --tmpfs /out:size=256k`). A genuine `ENOSPC` from the kernel, inside a container — never the developer's own drives. |

```json
{ "code": "cli.failed",
  "message": "the drive holding /out/bundle/… filled up before the work finished",
  "hint": "Free space there and run this again. A build writes to two places — the output folder and the package store — and either can be the one that filled; a full bundle for a desktop target can be several gigabytes.",
  "exit_class": "environment", "exit_code": 2, "retryable": true }
```

Before: *"the disk filled up before the work finished"*, with no indication of
**which** disk. A build writes to the output folder and to the package store,
potentially on different drives, so that sentence is half an instruction. The
path is now shaped from its *head* rather than its tail — the tail was
`.tmp-3630954993`, a temporary name the operator has never seen.

**On screen.** Build screen error state, Retry offered.

**Re-driven through the UI: PASS, with one mis-routed action.** Caused by
pointing the build screen's output folder at a **256 KiB tmpfs**
(`docker run --tmpfs /tiny:size=256k`), which is a genuine `ENOSPC` from the
kernel inside a container and nowhere near a real drive. The banner reads *"the
drive holding /tiny/ubuntu-24.04-amd64-20260907/… filled up before the work
finished"* — the path shaped from its **head**, which is what this row asked
for — with the hint verbatim below it and `debark class environment (exit 2)`
as supporting text. **Build again** is offered and is the right action.

**The mis-route, reported not fixed.** The action bar also offers **"Open
readiness"**, because `build.js` routes every `environment`-class failure
there. That is right for §2.4 and §3.9, whose remedies are readiness rows, and
wrong here: readiness has no disk-space check and nothing on that screen helps.
Telling the two apart needs something on the `UIError` that distinguishes "this
machine cannot do the work" from "this machine ran out of room", and there is
nothing — both are `cli.failed`, class `environment`, `retryable: true`. Reading
the message text to decide would be the exact thing this document forbids. It
is one extra secondary button pointing somewhere unhelpful, next to a primary
action that is correct, so it is recorded rather than papered over.

### 3.7 The operator presses Stop

| | |
|---|---|
| **Caused by** | `StartBuild`, then `CancelBuild()` four seconds in. |
| **Happens** | `build:finished` arrives with `ok: false` and **`error: null`**. `BuildStatus().Cancelled` is `true`. No bundle folder was left behind. |

**There is no `UIError`, and that is correct.** The operator's own decision is
not a failure of the work, and the taxonomy has no cancellation class
(`cli-surface.md`, "what the CLI does not give the GUI", item 1) — so the
adapter exposes `Canceled()` and `internal/app` sets `Cancelled` rather than
inventing one.

**On screen.** A neutral "Cancelled" state, **not** an error banner and not a
red anything. Offer "build again". Say plainly that nothing was written. A
screen that branches on `ok === false` alone will get this wrong; branch on
`status.cancelled` first.

**Re-driven through the UI: PASS, and the accessibility finding is fixed.**
Caused by pressing **Stop the build** four seconds into a real build and
confirming. The screen shows **Stopped**, a *warning*-toned banner — not a red
one — titled "You stopped this build" with *"Nothing here failed. debark has
no exit class for cancellation, so this run is recorded as stopped."*, and the
phase strip trails "stopped by you here". **Build again** and "Change the
options" are offered. Nothing on the screen calls it an error.

**The confirmation now closes when its subject is over.** `docs/accessibility.md`
§10 carried "a confirmation must close when its subject is over", found when
the "Stop this build?" dialog stayed open after the build it was asking about
had finished. Driven directly: the dialog was opened four seconds in and left
unanswered while the build ran to completion. It **closed by itself** and focus
returned to the action bar, which was visibly ringed on the next capture.

**One correction to this row.** It asks the screen to "say plainly that nothing
was written", on the strength of a second-round run that left no bundle folder
behind. The screen says something more careful and more useful — "The output
folder may hold a partial, unsigned bundle. Do not copy it to a target; build
over it or delete it", alongside "Downloads that completed are in the local
store and will be reused" and "Nothing was written to the target." Whether a
partial folder survives depends on where in the run the stop landed, so a flat
"nothing was written" is a promise the application cannot keep. Corrected: what
this row requires is that the screen says what the stopped run left **on this
machine** and confirms that the target is untouched.

### 3.8 A package name the target's archive does not carry

| | |
|---|---|
| **Caused by** | `StartBuild` with `jq` and `no-such-package-xyzzy`. |

```json
{ "code": "cli.failed",
  "message": "apt: could not satisfy requested packages: no-such-package-xyzzy (unable to locate package)",
  "hint": "Check the package names against the catalogue for this target. A package the target's archive does not carry has to be added as a vendor .deb URL or a local file instead.",
  "exit_class": "resolution", "exit_code": 5, "retryable": true }
```

Unchanged, and left alone deliberately: debark's own line already names the
package, which is more than a rule could add. Contrast with 3.1 — the same
class, the same hint, and there the hint was wrong because the names were not
the operator's.

**On screen.** Route back to the picker with the offending name highlighted.

**Re-driven through the UI: PASS.** Caused by pasting `jq` and
`no-such-package-xyzzy` into the picker's "Paste a package list" and building.
The banner is debark's own summary verbatim — *"apt: could not satisfy
requested packages: no-such-package-xyzzy (unable to locate package)"* — with
the hint under it, and the action bar offers **Back to packages**, "Change the
options" and **Build again**. Following the route lands on the picker with the
tray showing `no-such-package-xyzzy` in a warning-coloured chip marked
**UNKNOWN**, above the summary "1 not in the catalogue". That is the
highlighted offending name this row asks for.

**One honest caveat.** The highlight is the tray's own pre-existing state — the
name is marked unknown because the catalogue does not carry it, not because the
build failed on it — and the paste dialog had already warned before the build
ran. So the route works, and it would not highlight a name that *is* in the
catalogue but that apt still could not satisfy (a version pin, a broken
dependency). That case was not caused here.

### 3.9 A build from Windows, where apt has to run in a container

| | |
|---|---|
| **Caused by** | `StartBuild` on the Windows host with Docker Desktop running Linux containers, and no `bin/debark-linux-amd64` beside the executable. |
| **Happens** | The container backend mounts a debark binary into the container and re-execs it. On a non-Linux host the executable it reaches for is the `.exe`. |

```json
{ "code": "cli.failed",
  "message": "this build has to run apt inside a Linux container, and the only debark it can find to put in that container is …\\debark\\debark.exe, which is not a Linux program",
  "hint": "Put a Linux build of debark named bin/debark-linux-<arch> beside the debark this application runs — that path is found with no further setting — or run the build on Linux, or on WSL. Nothing is wrong with the target you picked; this machine cannot reach it from here.",
  "exit_class": "environment", "exit_code": 2, "retryable": true }
```

Before, the message was debark's own: *"container: C:\…\debark.exe does not
look like a Linux ELF binary (bad magic number '[77 90 144 0]' in record at
byte 0x0)"*, and the hint a `CGO_ENABLED=0 GOOS=linux go build …` command line.
A magic-number dump in an error dialog reads as a corrupt file, which is the
wrong worry entirely.

**Still true, and worse than a failed build.** The readiness screen reports
`can_build: true` on this machine, with Docker present and no blocking row, and
then every build fails. Readiness cannot know: nothing it can probe
distinguishes "docker works" from "debark can use docker from here". Core
`a0b61c2` gives the operator a way out that did not exist before; it does not
give readiness a way to check. Suggested for `internal/readiness`: on a
non-Linux host, check for `bin/debark-linux-<arch>` beside the debark
binary, `DEBARK_SELF_BINARY`, and the `self_binary` config key, and warn when
none is present.

**Not driven in this pass, and it is the one row of section 3 that was not.**
It is Windows-only by construction: it needs the GUI and `debark.exe` built
for `windows/amd64`, Docker Desktop running Linux containers, and no
`bin/debark-linux-amd64` beside the executable. Every other row here was
driven inside a container against a Linux binary, and this one cannot be — the
whole failure is that the executable is a PE file.

What that leaves unverified is narrow, and worth saying precisely. The
`UIError` is real and was captured on the Windows host in the second round. Its
**rendering** is the build screen's `failed` outcome for an `environment`-class
error, which is the same code path as §2.4 and §3.6, both driven this pass and
both rendering `message` and `hint` verbatim with an "Open readiness" route —
and unlike §3.6, readiness *is* the right destination for this one, because
that is where a container-runtime row lives. So the row's fourth column is
covered by construction rather than by observation, and this pass says so
rather than claiming a screenshot it does not have.

### 3.10 The options do not make a runnable command

This row is last in the section and happens before everything else in it. It
is also the only row in this document where **nothing ran and nothing will**,
which is why it took a decision to admit rather than a driving session to find.

| | |
|---|---|
| **Caused by** | `PreviewCommand` with a vendor URL carrying a query string *and* an expected SHA-256: `https://127.0.0.1:9392/hello.deb?token=abc123` with a 64-hex digest. Driven through the picker's "Add a vendor .deb by URL" dialog and the build screen's command panel. |
| **Happens** | `build --digest` is pflag `stringToString`, which splits a `URL=SHA` pair at the **first** `=`, so a URL with a query string silently loses its digest. `BuildSpec.Validate` refuses the spec rather than letting that happen quietly. |

```json
{ "code": "cli.failed",
  "message": "the expected digest for https://127.0.0.1:9392/hello.deb?token=abc123 cannot be passed to this debark, because the URL contains \"=\"",
  "hint": "`build --digest` splits the pair at the first \"=\", so a URL with a query string loses its digest without saying so. Either clear the expected SHA-256 for this one URL and accept an unverified download — which is what would silently have happened — or use a URL with no query string, or download the file yourself and add it as a local .deb, where the digest is checked on disk. A newer debark whose `build --help` says --digest is split at the LAST '=' does not have this limit.",
  "exit_class": "usage", "exit_code": 1, "retryable": false }
```

Every other `Validate` refusal reaches the same place: no target, both a
snapshot and a base, an empty selection, an empty URL row, a SHA-256 that is
not 64 hex characters, a key and "unsigned" together.

**On screen — and this is a render site no other row covers.** It belongs in
the **command panel**, in place of the command, because that is the thing the
refusal is about. It is a `warning`, not a danger: nothing has been damaged and
nothing has been attempted. It must **gate the primary action**, and the setup
bar's status line must say so, because that line is the only place the reason
Build is unavailable is written down.

**Do not render `exit_class` or `exit_code` here at all.** They are the
application's own classification of a spec it declined to run; no process
exited and there is no stderr. A screen that shows "exit 1" for this is
reporting a program failure that did not happen — which is the trap the fourth
column's first rule warns about, arriving from a direction that rule did not
anticipate.

**Why this earns a row, recorded because the question was asked.** The other 21
are failures of something that ran; this one is not, and that difference is
real. It earns a row anyway, for three reasons. First, this document's subject
is *"every failure this application can put in front of an operator"* — a
definition about what a person sees, not about whether a subprocess exited.
Second, it is the **only** row whose render site is the command panel, and
every other row lands in a banner, a readiness row or an outcome panel; a
render site with no specification is a render site nobody checks, which is
exactly what happened. Third, it is the failure an operator meets **most
often**, because the build screen opens in a state that produces it.

**DEFECT, found by writing this row and now fixed.** `PreviewCommand` did not
call `spec.Validate()`, so the refusal never happened and the operator met the
problem on Build instead. Another change fixed the call. That exposed the half
in this package files: `syncBuildEnablement` computed its gating from local
form state only and never looked at `CommandPreview.Error`, so on a refusal the
command block went empty while the status line still read *"Ready. The command
above is exactly what will run"* and Build stayed `aria-disabled="false"`.
`startBuild` refused it, so nothing dangerous happened — the one sentence whose
job is to explain why the primary action is unavailable was saying the
opposite. Driven after the fix: the status line reads **"To start: fix what the
command panel reports."**, Build is `aria-disabled="true"`, and removing the
offending URL restores both.

**Two smaller findings from the same run, reported not fixed.** The Add-URL
dialog **approves what the preview then refuses** — it showed the green "This
download will be operator-attested" for the very URL `Validate` rejects, so the
refusal arrives one screen later than it could. And the dialog **keeps the
previous URL and its digest when it is reopened**: the address is preselected
so typing replaces it, but the SHA-256 is not, so a second vendor URL silently
inherits the first one's digest — which manufactures §3.4.

---

## 4. Verifying a bundle

`verify` was the second silent-failure command until core `3ed80bd`. It now
prints its first problem on stderr, which is why 4.2 and 4.3 improved without
anything in this repository changing.

### 4.1 A bundle whose contents were altered

| | |
|---|---|
| **Caused by** | `StartVerify` on a copy of a freshly built, signed bundle with **one byte appended** to `repo/Packages`. |

```json
{ "code": "verify.failed",
  "message": "the bundle did not verify: file size does not match the manifest",
  "hint": "The bundle is not byte-for-byte the one that was signed, so it must not be installed. Copy it again from the machine that built it; if a fresh copy fails on the same file, treat the media it travelled on as suspect.",
  "exit_class": "verification", "exit_code": 4, "retryable": false }
```

**On screen — reach for `VerifyStatus.Findings`.** The `UIError` names the
problem but not the file; the findings do, and each carries its own hint from
the engine:

```json
{ "severity": "error", "code": "file-size-mismatch",
  "message": "file size does not match the manifest",
  "hint": "Expected 2784, found 2785.", "path": "repo/Packages" }
```

Render the findings as a list under the banner, each with `path`, `message` and
its own `hint`. A verification screen that shows only the banner throws away
the only precise thing in the report.

**Re-driven through the UI: PASS.** Caused end to end: build a signed bundle,
export it, append one byte to `repo/Packages` in the copy, press **"Check the
copy with `debark` verify"** on the export screen. The banner is
*"the bundle did not verify: file size does not match the manifest"* with the
hint verbatim, and under it, exactly as the row asks, the finding as its own
row:

```
file size does not match the manifest
repo/Packages
Expected 3062, found 3063.
```

— `message`, `path` and the engine's own per-finding `hint`, all three. A
"What failed" drawer holds the argv and the raw stderr.

**One thing that had to be arranged before this row could be driven at all, and
it is a product gap worth reporting.** The first attempt produced §4.3's
message instead: `verify` checks the signature before it checks any file, and
the signature failed, because the application had passed no `--key`.
`internal/app`'s `verifyKeys` looks for a public key beside **`a.keyPath`**,
which is fixed at construction to `readiness.DefaultSigningKeyPath()` and is
**never updated from the build screen's signing key**. So a bundle signed with
a key the operator typed into the build screen can never verify in this
window, and the operator is told the key is missing rather than that it was
never offered. Driven around by copying the key pair to the readiness default
path, after which this row reproduces exactly. Reported for `internal/app`, not
fixed here.

### 4.2 A folder that is not a bundle

**This row changed exit class under this pass, and it is now two failures.**
Core `5837d82` moved "a folder that is not a bundle" to **1 (`usage`)** with
**zero bytes on stdout**, so there is no `--json` report to parse and no
findings list to render. What stayed at exit 4 is a **real bundle whose
manifest was deleted**, which genuinely is tampering. The boundary is
structural and core draws it by asking whether the directory carries any of a
bundle's parts — the manifest, its signature, `lock.json`, `snapshot.json`,
`repo/`. `README.txt` is deliberately not a marker.

```json
{ "code": "cli.failed",
  "message": "verify: /media/usb is not a debark bundle: it has no debark.manifest.json and none of a bundle's other parts",
  "hint": "Pick the bundle folder itself — the one `debark build` wrote, containing debark.manifest.json and lock.json — rather than the drive's root or a folder beside it.",
  "exit_class": "usage", "exit_code": 1, "retryable": false }
```

**On screen.** A correction, not an alarm: name the folder, say what a bundle
folder looks like, and offer the picker again. There is no findings list and
there must not be one — `verify` wrote nothing to parse. Do **not** render the
exit code as the headline; nothing failed verification, because nothing was
verified.

**Re-driven through the UI: PASS, end to end, and the rule the previous pass
added is confirmed.** Driven on the export screen by stripping the exported
copy down to a lone `README.txt` and its `last-run-*.txt` files and pressing
"Check the copy". The screen renders the message and hint above verbatim, with
no findings list, and the drawer confirms `usage · exit code 1`. Without the
`verify/not-a-bundle` rule this falls through to the usage-class floor, which
tells the operator to check for a version mismatch between the app and the
debark it is driving; they picked the wrong folder. `README.txt` being
present did not stop it, which is the core rule holding.

**The boundary was driven too, and it exposed a defect that is now fixed.**
Deleting only `debark.manifest.json` from a real signed bundle still exits
**4** — confirmed in the drawer, `verification · exit code 4` — and the screen
answered *"Pick the bundle folder itself … rather than the folder above it."*
They had picked the right folder. `internal/cliadapter`'s
`verification/no-manifest` rule was still written for the world before
`5837d82`, where the two cases shared an exit code and therefore a sentence.
It now says that the one file everything is checked against is gone, that
nothing here can be checked, and that a copy which stopped part-way looks
exactly like this — and the wrong-folder advice stays where the wrong folder
now lands, at exit 1. `TestClassify`'s table carries both sides so neither can
be softened without the other failing.

### 4.3 A bundle signed with a key this machine does not trust

| | |
|---|---|
| **Caused by** | `StartVerify` on an untouched, correctly signed bundle whose operator **public** key was deleted. |

```json
{ "code": "verify.failed",
  "message": "the bundle did not verify: no signature verifies against a trusted key",
  "hint": "Nothing here trusts the key this bundle was signed with. If you built it on this machine, the operator public key that was written beside the signing key is missing — the readiness screen names the path it looks at. If it came from someone else, get their public key by a route other than the media the bundle arrived on, and add it to the trusted keys.",
  "exit_class": "verification", "exit_code": 4, "retryable": false }
```

Before, this used debark's own hint: *"pass it with `--key`, or list it under
`verify_keys` in the config file"*. Both are right at a terminal and neither is
reachable from this window — the application already passes the public key it
finds beside the signing key (`cliadapter.KeyedVerifier`), so the actionable
fact is that the file is missing.

**On screen.** Do not soften this: an unverifiable bundle must not be
installed. But say which of the two situations it is, because a bundle you
built yourself failing this way is a missing file, not an attack.

**Re-driven through the UI: PASS.** Caused on an untouched, correctly signed,
exported bundle by deleting the operator **public** key from the path
`internal/readiness` names, then pressing "Check the copy". The banner is
*"the bundle did not verify: no signature verifies against a trusted key"* with
the hint verbatim — including both halves, "if you built it on this machine…"
and "if it came from someone else…", which is exactly the distinction this row
asks for. Under it, the finding: `no signature verifies against a trusted key`
against `debark.manifest.sig`. The drawer shows the argv with
`--key /sp/home/.config/debark/operator.pub` when the key is present and
without it when it is not, which is the one fact that tells the two situations
apart and it is one click away.

---

## 5. Copying to a drive

### 5.1 The drive is pulled out mid-copy

| | |
|---|---|
| **Caused by** | A genuine device-level I/O failure. A 1.4 GB loop-backed volume behind a device-mapper *linear* target, `mkfs.ext4`, mounted; the export started; then, 0.4 s in, `dmsetup suspend --noflush --nolockfs` + `reload` with the **`error`** target + `resume`. Every read and write to that volume then returns `EIO`, which is what a removed drive does. All of it inside a privileged container, on a loop file in that container's own filesystem — no real device, no removable media, nothing outside the container touched. The loop device and dm node are kernel objects in the shared WSL2 VM, so the script removes both on every exit path; both were verified gone afterwards. |

```json
{ "code": "export.failed",
  "message": "The drive stopped responding while writing /media/usb/bundle/repo/pool/padding.bin — it looks like it was unplugged.",
  "hint": "Plug the drive back in, wait for the system to mount it, and export again from the start. Do not use what is on the drive now.",
  "details": "flush /media/usb/bundle/repo/pool/padding.bin: sync /media/usb/bundle/repo/pool/padding.bin: input/output error",
  "retryable": true }
```

**This one needed no change.** `internal/export` classifies the OS error and
writes its own summary and hint, and both are exactly right. Recorded here
because the definition of done asks for the whole chain, and this is the chain
working.

**On screen — reach for `ExportStatus`.** It carries what the banner cannot:

```json
{ "progress": { "phase": "failed", "file": "repo/pool/padding.bin",
                "files_done": 14, "files_total": 16,
                "bytes_done": 705047584, "bytes_total": 839272512, "fraction": 0.84 },
  "destination_path": "/media/usb/bundle",
  "summary": "The export stopped after 14 of 16 files (839 MB). …" }
```

Show how far it got and that the destination is unusable. `retryable` is true
and means "start again from the beginning", not "resume".

**Re-driven through the UI: PASS, and it is the most complete rendering in this
document.** Re-caused the way this row describes: a 1.2 GB loop file inside the
container, a device-mapper **linear** target over it, `mkfs.ext4`, mounted at
`/media/usb` — where the export screen listed it under **"REMOVABLE MEDIA — THE
DRIVES YOU CAN PULL OUT"**, which is the classification working — a 250 MiB
`padding.bin` in the bundle so the copy is genuinely in flight, then 0.45 s
into the copy `dmsetup suspend --noflush --nolockfs` + `reload` with the
**`error`** target + `resume`. The loop device and the dm node were removed
afterwards and both were verified gone.

The screen showed, in this order:

- **The message verbatim**, naming the file it was on: *"The drive stopped
  responding while writing
  /media/usb/ubuntu-24.04-amd64-20260907/repo/pool/padding.bin — it looks like
  it was unplugged."*
- **The hint verbatim**, and a "Copy the message" action.
- **A "What failed" drawer.**
- **How far it got**, from `ExportStatus.progress`: *"It stopped after 13 of 16
  files, 234 MiB of 250 MiB written."*
- **That the destination is unusable**, as a danger banner of its own: *"This
  drive now holds an incomplete bundle — do not use it"*, naming the
  `.debark-export-incomplete` marker, saying the export writes it before its
  first byte and only a passing verification removes it, and telling the
  operator to export again over it or delete the folder before taking the drive
  to the offline machine.
- **"Copy and verify"** as the action, which is "start again from the
  beginning" and not a resume.

Two attempts were needed and the first one is worth recording, because it is a
trap for whoever re-causes this: flipping the target at **1.6 s** into a 2 s
copy produced a **successful** export. The writes were still in page cache and
the verification read them back from cache, so the `EIO` never reached the
process. 0.45 s into the same copy is inside the big file and the failure is
real.

### 5.2 A destination with not enough room

Not driven as a separate row: `PlanExport` computes `required_bytes`,
`free_bytes` and a `margin_bytes` before any copy starts, and the export screen
should refuse ahead of time rather than fail half way. The plan values were
captured in the run above and are real.

**Driven as its own row now, through the UI: PASS.** Caused with the same
apparatus as §5.1 at a different size — a 32 MiB loop-backed ext4 volume, 23.7
MiB free, against a 397 KiB bundle. The export screen **refused before writing
anything**, with the numbers in the sentence rather than behind a drawer:

> The drive does not have room for this bundle: it needs 64.8 MiB (397 KiB of
> files plus a 64 MiB safety margin) but only 23.7 MiB is free.

and beneath it *"Free up 41 MiB on the drive, or choose a larger one, then
export again."* with a **Check again** action. The plan panel above it is
headed "What would happen — worked out without writing anything, so it is safe
to read before you commit", and carries files, bundle size, room needed, free
space and the exact path that would be written.

**On screen, therefore — this row had no fourth column and now has one.** The
refusal must state all three numbers (needed, of which margin, and free), the
shortfall as a single figure the operator can act on, and a way to re-check
without re-choosing the drive. It must sit where the plan sits, before the
action, and the action must be unavailable rather than merely discouraged.

---

## What could not be caused on this machine

Stated plainly, because a catalogue that quietly omits what it could not test
is worse than one that is short.

- **A snapshot captured on a genuinely different architecture.** Docker on this
  host has no `binfmt_misc`/qemu registration: `docker run --platform
  linux/arm64 debian:bookworm-slim` fails with `exec /usr/bin/sh: exec format
  error`, so no arm64 container could be started to run `snapshot create` in.
  What was driven instead: a **release** mismatch (§2.4), which takes the same
  code path in `core/apt`'s backend selection and is what an operator on a
  Debian builder aiming at an Ubuntu target actually hits. Two related findings
  from trying: building `--base debian:12/minimal --arch arm64` on an amd64
  host **succeeds** — multi-arch resolution works, so a cross-architecture
  target is not in itself an error — and the wrong-architecture failure that
  does exist (exit 7, `target-mismatch`) happens at **install** time, on the
  offline target, which this application never runs. To cover it properly:
  register `qemu-user-static` binfmt handlers, or an arm64 machine.
- **`install` failures generally.** `cliadapter.Adapter` has no `Install`
  method by design; the offline side is the CLI's job. The `target/bundle-for`
  rule is therefore marked *not driven* in the code — it was updated only
  because core `3ed80bd` changed the wording from `;` to `,` and silently
  broke the pattern, which was read out of `internal/cli/cmd_install.go` at
  `6549f6f` rather than observed.
- **A full disk on the machine's real drives.** Deliberately not attempted.
  This is a live working machine running file-recovery jobs. The tmpfs in §3.6
  is a real `ENOSPC` from the kernel and is confined to a container.
- **A physically removed USB drive.** Also deliberately not attempted, for the
  same reason. §5.1's device-mapper `error` target produces the same `EIO` the
  kernel gives for a vanished device, at the same layer, and is confined to a
  loop file inside a container. Re-caused this pass with the same apparatus,
  and the loop device and dm node were removed and verified gone afterwards.
- **The Windows-only build failure (§3.9).** It needs a `windows/amd64` GUI and
  `debark.exe`, Docker Desktop, and a missing `bin/debark-linux-amd64`. The
  whole failure is that the executable is a PE file, so it cannot be caused in
  the Linux container every other row was driven in, and running the app on the
  developer's live desktop to get it was not worth the disruption. Its row says
  which driven row covers its rendering and why.
- **A name that IS in the catalogue and that apt still cannot satisfy** — a
  version pin, a broken dependency. §3.8 was driven with a name the catalogue
  does not carry, which the picker already marks. The route back to the picker
  works either way; whether the *offending name* is highlighted was only shown
  for the case the tray already knew about.

---

## Core-repository gaps this exercise found

**Four of the five are now fixed.** They are kept here rather than deleted,
with what closed them, because a gap that was worth naming is worth being able
to check later — and because two of them left a second half of the work inside
*this* repository, which is the part that would otherwise be lost.

1. **FIXED, core `1c767f7`. `BuildResult.FetchFailed` carried no reason.** An
   unreachable host, a SHA-256 mismatch and an untrusted TLS certificate were
   indistinguishable in the result document; only a warn-level `input.external`
   event told them apart. `BuildResult` now carries `fetch_failures[]` of
   `{input, reason, detail}` beside the untouched `fetch_failed[]`, twelve
   reasons in a closed set, tagged at each of 34 return sites. **Half of this
   is still open in this repository, and it is small:** `internal/app`'s
   `BuildSummary` projects only `fetch_failed[]`, so the frontend still reads
   the reason out of the event's English rather than off the classified field.
   See §3.3–3.5.
2. **FIXED, core `d5f31cb`. `apt.update` reported `failed: false` when nothing
   could be fetched.** It now carries `sources_fetched`/`sources_failed`, and
   re-driving `--network none` against the new binary gives
   `{"sources_failed":4,"sources_fetched":0}` where it used to give nothing at
   all. It deliberately still does not fail the run — apt is the oracle
   (ADR-001) — which is right, and means the *UI* has to say it. It now does,
   in `build.js`. See §3.1.
3. **FIXED, core `9c77bbe`. `core/sign/ed25519.go` named a command that does
   not exist.** The hint for a missing private key said *"generate one with
   `debark key generate`"*; it now says `debark keygen --out PATH`.
   Confirmed on the stderr of a re-driven §3.2.
4. **FIXED, core `5837d82`. `verify` on a folder with no manifest was
   classified `verification`.** A directory that is not a bundle now exits 1
   (`usage`) with zero bytes on stdout; a real bundle whose manifest was
   deleted still exits 4, because that genuinely is tampering. **The second
   half was in this repository and is done:** the recognition rule still spoke
   as though the two were one failure and told the operator of a *tampered*
   bundle that they had picked the wrong folder. See §4.2.
5. **STILL OPEN. Readiness cannot tell whether the container backend will
   work.** `a0b61c2` made the Windows case fixable; nothing yet makes it
   *checkable*, so the readiness screen still says `can_build: true` on a
   machine where every build fails. See §3.9. Partly a `debark` question and
   partly `internal/readiness`'s. This is the last one, and it is the only gap
   in this list that another round has not closed.

### And one gap that is not core's, found by the rendering pass

**A vendor URL's credential reaches the build screen and a clipboard.**
`BuildResult.FetchFailed` is the operator's literal input string and core
deliberately does not redact it — sound for a result document, and not for a
list rendered under a "Copy this list" button after every neighbouring surface
has stopped doing it. The same disagreement also breaks the join that puts the
reason beside the input, because the event stream *is* redacted. One call to
`fetch.RedactURL` in `internal/app`'s `BuildSummary` projection fixes both.
See §3.3–3.5.

### And two corrections owed to `docs/dev/cli-surface.md`

That document is owned by another package, so these are reported rather than
made:

- **C1 is no longer true.** A failing `build` writes its reason to stderr as of
  core `3ed80bd`. Same for `verify` (**C7**), `install` and `fetch`. The exact
  new `build` shape is
  `build: <bundle>: <class>[: N input(s) apt could not satisfy: …][; N URL(s) that did not download: …]`,
  from `internal/cli/cmd_build.go`'s `buildFailureError`, with a hint on the
  incomplete class. Lists elide after three names.
- **C8 is fixed.** `snapshot inspect` on a structurally wrong file exits 1
  (`usage`) as of `2d6d5c7`. **Content** failures — a truncated archive, a
  digest mismatch, a path escape — correctly keep exit 4, and §2.1 versus §2.2
  is the boundary in practice.

`cli-surface.md` §6 (the frozen 0–7 exit-code table) and every `--json` type
were re-checked against `6549f6f` and are unchanged.

---

## What the rule table now is, and how to add to it

`internal/cliadapter/errors.go` holds the recognition table. Adding a
newly-recognised failure is one row — a class, an optional command, a regexp, a
summary template, a hint — and it cannot break the fallback: `errResolve` has
one return, and everything reaching it has been through `errFloor`, which is
total over every class and every exit code. A rule that does not match, or that
matches but captures an empty group, simply does not fire.

Three conventions the redesign should know about:

- **`hintWins`** marks a rule whose hint replaces debark's own. It is used
  where the CLI's hint is aimed at a developer at a terminal rather than an
  operator at a window — a flag they cannot reach, a `go build` command line, a
  config file they do not edit. Most of the improvements in this catalogue are
  exactly that.
- **A rule may improve only the hint.** Several rows above keep debark's own
  summary, because it names a file or a package that no template could add, and
  replace only the advice.
- **A rule that has never been seen to fire says so in a comment.** Two do.

**Do not add a screen-level workaround for anything in this document.** If the
engine did not say what the UI needs, it is a core-repo gap and belongs in the
list above.
## Desktop simplification: final native rendering appendix

Pending coordinator completion against the final rebuilt Wails binary.
For each affected error case above, record its visible entry action, reached
binding/job, exact safe UIError projection, source/binary identity, screenshot
or journal, observed remedy and pass/fail. Include real preparation failure,
Undo expiry, signing failure, close timeout, incomplete build, copy mismatch,
unplug/cancel and verification opt-out. Fixture-seeded outcomes and automated
Go tests remain separately labelled; neither establishes this native journey.
