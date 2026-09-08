# Accessibility — what was tested, with what, and what it found

> The capture journals, screenshots and raw logs cited in this document are
> part of the project's internal records and are not published with the
> source. The measurements they back are reproduced here in full.

## Desktop simplification: current coverage, 2026-09-07

The three-stage desktop workflow supersedes the presentation described in the
historical third-round record below. Earlier Orca transcripts remain evidence for
their recorded binary; they do not certify the redesigned screens. Current
behavior is described in [the operator guide](user-guide.md) and the
[frozen UX contract](dev/ux-contract.md).

Implemented accessibility behavior includes visible three-stage navigation,
one screen action area outside its scrolling content, explicit blockers on
primary actions, and native-compatible disclosures. Packages uses a fixed
32px virtual list: Space selects, Enter opens a named Details dialog, and
closing restores the logical row while query and target are unchanged. Add
remains available with an empty selection. The selected-items pane becomes a
named dialog below 1024 effective CSS pixels; enlarged text participates in
that decision. Removing the last item and expiring Undo have a visible focus
fallback. These are implementation facts, not screen-reader results.

| Current evidence | Result and limit |
|---|---|
| S2 target Continue journal | Visible Continue committed one target and called preparation once on WebKitGTK 2.52.6, Ubuntu 24.04.4, 1040×720. Fixture bindings, not a native chooser or live archive. |
| S2 Packages search journal | Visible search reached the actual screen at 1040×720 with no recorded JavaScript/fixture error. This is a small fixture catalogue. |
| S2 Details journal | Enter reached `GetPackage("gimp")` and opened Details at 960×640 in dark theme. This establishes the fixture interaction, not Orca speech or native key input. |
| S2 Details-close attempt | Driver failed to match the accessible Close name. Opening passed; close/focus restoration remains unverified by this journal. Do not count the attempt as a pass. |
| Package preparation controller demo | Node execution passed cache reuse, deduplicated start, cancel/no automatic restart, explicit Retry, failure/no retry loop, and previous-target completion rejection. This test does not exercise layout or an accessibility API. |
| Native close policy | Go race tests cover prompt return, default Keep working, bounded cleanup, timeout/retry and late completion. The native appendix records the spoken Keep working default, actual Stop and close during an 8GiB copy, and close after a completed signed build. Native timeout/retry remains outside the captured walkthrough. |

S2 journals identify the exact source snapshot and content-manifest hash.
Their inspection fields must be completed alongside the final capture index;
they are not evidence for later edits automatically. Reproduction and fixture
limitations are in [desktop visual review](dev/ui-review.md).

The [final evidence appendix](#desktop-simplification-final-evidence-appendix)
adds the S3/01 60,000-row demo and screen journeys, including their failed
driver assertions. These are additional WebKit fixture observations; native
keyboard and speech acceptance remains separate.

**Final native acceptance is incomplete.** The appendix records actual Wails
and Orca observations, including unresolved package-list speech and ancestry.
For each further run, record the rebuilt Wails binary hash,
GUI/engine source identity, native client size, scaling, theme and actual input
method before marking any item complete. Required affected paths are main/Add
menus; three-stage and utility navigation; Space versus Enter; Details-close
focus after virtual-row recycling; removal/Clear/Undo with the pane hidden;
URL/digest, paste and multi-file dialogs; Advanced/Command and signing
validation; build/copy return and cancellation; native Quit's Keep working
default and Stop and close; minimum-size and enlarged-text action reachability.
Repeat the affected Orca/AT-SPI2, contrast and reduced-motion checks. Read-only
DOM geometry or a fixture-produced key event is not native keyboard or speech
evidence. Windows/WebView2/NVDA remains unmeasured.

## Historical third-round record

This was the record for the definition-of-done item
*"keyboard navigation and an accessibility pass, tested with a real screen
reader."*

It supersedes the second-round record. The second round tested screens that no
longer exist, in an engine the product does not ship, through an accessibility
API the product does not use. What survived is marked as such and the engine it
was taken on is named beside it. Read §1 first, then §5.

---

## 1. The honest headline: a screen reader was used, on the shipping engine

**Orca 46.1 was driven against the real Debark binary, rendering in
WebKit2GTK 2.52.6, over a live AT-SPI2 bus, and its speech was captured.** Every
transcript quoted in this document is a `SPEECH OUTPUT` line from Orca's own
debug log. Nothing here is a tree dump described as a screen-reader session.

That is the first genuine screen-reader evidence this project has had, and it
found five defects that no tree dump could have found — including one, in §5.3,
where the ARIA is correct, reaches the platform layer intact, and is still
never spoken, and one, in §5.7, where a fix this document itself signed off
restores focus to an element a screen reader declines to name.

**Nothing ran on the developer's desktop.** Everything is inside a container on
displays `:94`–`:96` and, for the later pass, `:140`–`:142`, against a binary
built inside that container. No assistive technology was started on the host, no
accessibility setting on the host was read or changed, and nothing was installed
outside the image.

### What this does not cover, stated as plainly as the above

- **Windows is untested.** No NVDA, no Narrator, no WebView2 session informed
  anything here. The second round's attempt was abandoned — a portable NVDA
  would not initialise over RDP and put a command-line error dialog on the
  owner's live desktop each time it was launched — and it was not retried.
  Windows is a target the app builds for; its accessibility is unmeasured.
- **Nobody listened.** Orca's speech went to the `dummy` speech-dispatcher
  module and was read out of the debug log. That captures *what* it says, in
  order, which is the thing tree dumps could never give. It does not capture
  rate, interruption, or whether a real user would find the result usable.
- **No braille display.** Braille was disabled. §5.5 records one artefact that
  is inaudible in speech and would be visible in braille.
- **~~Structural navigation was not exercised.~~** It has been now, on the real
  binary, and the worry that prompted the caveat was wrong: `H`, `D`, `T` and
  `L` all work, unmodified, in the shipping app. §12 has the transcript. What
  is still not exercised is browse mode *systematically* — four keys on one
  screen is a spot check, not a survey.
- **Nothing was tested with `prefers-contrast: more` turned on beyond the
  stylesheet.** §8.3 measures which tokens change and photographs the result;
  nobody has driven a whole keyboard walk with it on, and there is no reason to
  expect a difference, which is not the same as having looked.
- **One session, one machine.** These are not repeated measurements.

So the definition-of-done item is now:

> Keyboard navigation: **done, on both engines.**
> Screen reader: **done on Linux/WebKit2GTK/Orca. Not done on
> Windows/WebView2/NVDA.**

---

## 2. The rig

| | |
|---|---|
| Image | `debark-a11y:go126`, derived from the third-round harness image `debark-shots:go126` by adding `at-spi2-core`, `gir1.2-atspi-2.0`, `python3-gi`, `python3-pyatspi`, `dbus-x11`, `orca`, `speech-dispatcher` |
| OS | Ubuntu 24.04.4 |
| Engine | WebKit2GTK **2.52.6** (`libwebkit2gtk-4.1`), GTK **3.24.41** |
| Screen reader | Orca **46.1** |
| Bridge | AT-SPI2, over a session `dbus-daemon` + `/usr/libexec/at-spi-bus-launcher --launch-immediately` + `at-spi2-registryd` |
| Binary | the real `debark-gui`, built in the container with `-tags "desktop,production,webkit2_41"` |
| Display | `:94`–`:96` for the first pass, `:140`–`:142` for the later one (each pass's own; two Xvfb on one number kill each other) |
| Also driven | WebKitGTK **2.44.0** (`debark-css:webkit2.44`, Ubuntu 24.04 at GA) for §8.3's high-contrast check, because 2.44 is the oldest engine that can install the package |

Four things had to be true before any of this worked, and each cost time:

1. **The stock image has no accessibility bus.** The app logs `AT-SPI: Error
   retrieving accessibility bus address: … The name org.a11y.Bus was not
   provided by any .service files` and publishes nothing. A derived image and a
   launched bus are not optional.
2. **Orca 46 reads `user-settings.conf` as JSON, not INI.** An INI file kills
   it with `KeyError: 'default'` inside `settings_manager.activate`, before it
   ever registers with the bus — and it dies quietly enough to look like "Orca
   just does not speak".
3. **The window is found by title `Debark`**, never by program name, and must
   be `xdotool windowactivate`d before it paints.
4. **`WEBKIT_DISABLE_COMPOSITING_MODE=1`.** The container's X stack has no GL
   and WebKit takes it down with compositing on.

### Driving, not reading

Two harnesses, neither of them committed to this repository:

- **`drive.py`** — a WebKit2GTK host (GTK3 + `WebKit2` 4.1 via GObject
  introspection, ~90 lines) that loads a page in the *same engine the product
  embeds*, sends real key events through `xdotool` to the real window, and
  reads state back with `run_javascript`. Keys go through the X server, so they
  are trusted events: `:focus-visible` behaves and the engine's own default
  actions run. This is the WebKit2GTK equivalent of what the second round did
  over the Chrome DevTools Protocol.
- **`atspi.py`** — an AT-SPI2 client that dumps the tree the engine publishes:
  role, name, description, state set, object attributes (`posinset`, `setsize`,
  `xml-roles`, `live`, `current`, …) and relation set. This is the tree Orca
  consumes.
- **`eval.py`** — the same host, but it runs one JS snippet after load and
  prints the JSON the page hands back. This is how every computed-style and
  media-query number in §8.3 and §9.1 was taken: `run_javascript` against a real
  `WebKitWebView`, not a headless emulation.

The per-screen `*.demo.html` harnesses were used as-is. They matter because the
real app cannot reach the picker or a running build without a catalogue and a
network, and because they now mount into a bounded frame — the thing the
second round's biggest defect turned on.

**The rule that earned its keep again: drive it, do not read it.** Four of the
first five findings in §5 came from pressing a key and asking what happened,
and the fifth came from asking Orca to speak and getting silence. §5.7 is the
sharpest version of the same rule, because it is a correction to a fix this
document had already signed off: that fix was verified by reading
`document.activeElement` back, which says where focus *is*, and never by a
transcript, which is the only thing that says whether anybody was *told*.

---

## 3. Which engine each claim was taken on

The single most important thing to know about the second-round record is that
it ran in Chromium and UIA, and the product ships WebKit2GTK and AT-SPI, which
share no accessibility code at all. This table says who measured what.

| Claim | Second round (Chromium 152 + UIA) | Third round (WebKit2GTK 2.52.6 + AT-SPI + Orca 46.1) |
|---|---|---|
| Tab order, ring on every stop, no trap | five screens, full stop-by-stop table | real app + build, target, tray, picker re-driven; see §6 |
| Virtualiser cursor, `aria-activedescendant` | Chromium DOM | **re-driven; §7** |
| `aria-setsize` / `aria-posinset` reach the platform | UIA `SizeOfSet` | **AT-SPI `setsize=60000 posinset=1`; §7** |
| Dialog name, modality, focus trap, Escape | Chromium `<dialog>` | **re-driven on build + all three tray dialogs; §6.3** |
| Gated primary action stays in the tab order | Chromium DOM | **Orca: "push button grayed" + the reason; §6.2** |
| Reduced motion collapses every loop | `Emulation.setEmulatedMedia` | **real GTK setting; §8.3** |
| Forced colours hand the palette to the OS | `Emulation.setEmulatedMedia` | **does not exist on this engine; §8.3.** Linux signals `prefers-contrast: more` instead, and the design system now answers it — §8.3, §9.1 |
| A live region re-announces | not tested | **inert; §5.5, re-confirmed on the current tree** |
| A focus move announces where it landed | not tested | **only onto a landmark or a control; §5.7** |
| A progressbar reports progress | not tested | **the first tick only; §5.8** |
| Contrast | computed from `tokens.css` | **recomputed from the current `tokens.css`; §9** |
| Reflow at 320px | CSS reasoning, one component | **five screens × eight viewport/zoom cases; §8.1** |
| Zoom to 200% | not tested | **measured; §8.2** |
| What a screen reader says | **nothing — no session** | **Orca transcripts throughout** |

---

## 4. What Orca actually said

Abridged; `SAID:` lines are verbatim `SPEECH OUTPUT` from the debug log, with
Orca's key echo removed.

### The gated stepper, on the readiness screen with no target chosen

```
SAID: 'Skip to content link.'
SAID: 'About Debark push button.'
SAID: 'landmark Build steps'
SAID: 'List with 4 items.'
SAID: '1 Target push button.'
SAID: 'Packages push button grayed.'
SAID: 'Choose a target first. The catalogue is built from its own apt indexes.'
SAID: 'Build push button grayed.'
SAID: 'Choose a target first, then pick the packages to include.'
SAID: 'Export push button grayed.'
SAID: 'Choose a target and build a bundle first.'
SAID: 'leaving list.'
SAID: 'Readiness , one check is blocking a build push button.'
```

This is the answer to "decide and document what the stepper is", and it is not
a design argument — it is what the shipping stack does with the markup that is
there. **The stepper is a `nav` landmark containing an ordered list of four
buttons**, and on this engine that gives an operator, from the accessible name
and state alone: the group ("landmark Build steps"), how many steps there are
and where they are in them ("List with 4 items", then the item), the step's
number and name, whether it is available ("grayed"), and why not — read from
the `aria-describedby` sibling. That is the whole requirement, met.

It is deliberately **not** a `tablist`. A tab set implies its panels are
siblings that already exist and are freely reachable; three of these four are
gated on work that has not been done, and `aria-disabled` on a `tab` is a state
that ARIA gives no meaning to. It is not a set of links either: a link that
refuses to navigate is a lie, and the refusal here has to be able to say
something and move focus.

`aria-current="step"` is on the current step, is correct, reaches AT-SPI — and
is never spoken. That is §5.3.

### The build screen's setup form

```
SAID: 'landmark Where the bundle goes'
SAID: 'About the output folder, name and format collapsed push button.'
SAID: 'Output folder entry /home/you/bundles.'
SAID: 'Choose… push button.'
SAID: 'Bundle name entry.'
SAID: 'Format combo box.'   SAID: 'A bundle folder.'
SAID: 'landmark Signing'
SAID: 'How to sign combo box.'   SAID: 'Sign it with a key.'
SAID: 'landmark Build options'
SAID: 'Write an SBOM sbom.cdx.json, CycloneDX. check box checked.'
SAID: 'Add a full-upgrade pass Also refreshes packages the target already has. check box not checked.'
SAID: 'landmark Redistribution'
SAID: 'I have read this and I am responsible for what this bundle redistributes. check box not checked.'
SAID: 'landmark The command this will run'
SAID: 'Copy the command push button grayed.'
SAID: 'Back to packages push button.'
SAID: 'Build the bundle push button grayed.'
SAID: 'To start: choose an output folder; give a signing key, or choose to
       write an unsigned bundle; accept the redistribution notice.'
```

Every group is a named landmark, every control has a name that is not its
placeholder, both gated actions announce as gated, and the reason for the gate
follows the button. §10's items 4, 9, 10 and 11 are all visible in this one
trace, working.

### The target step

```
SAID: 'landmark Choose a target.'
SAID: 'landmark How do you want to name the target?'
SAID: 'A stock base OS An assumption about a default install of a release you
       name. Available now; may be short.'   SAID: 'selected radio button'
SAID: 'landmark Which base?'
SAID: 'Architecture panel.'   SAID: 'amd64.'    SAID: 'selected radio button'
SAID: 'Distribution panel.'   SAID: 'Debian.'   SAID: 'selected radio button'
SAID: 'Release panel.'        SAID: '12 bookworm.'  SAID: 'selected radio button'
SAID: 'Variant panel.'        SAID: 'minimal.'  SAID: 'selected radio button'
SAID: 'About these four choices collapsed push button.'
SAID: 'Use a snapshot instead push button.'
```

Four roving-tabindex radio groups, each **one** tab stop with a name, and the
selected member announced. §10 P3-13 verified on the shipping stack.

Note `landmark Choose a target.` — that is what an operator hears when the
stepper takes them to a new step. It is not the shell's live-region
announcement; it is Orca reading the landmark that the shell moved DOM focus
into. See §5.5, because the difference matters.

---

## 5. Defects found this round, and the fix

Six, plus an artefact left alone and two mechanisms that turned out to work.
§5.1–§5.4 are fixed in this repository and re-driven on the engine that found
them. §5.5 cannot be fixed here and is recorded instead. §5.6 is the artefact.
**§5.7 is a real defect in a file this pass does not own, and is reported, not
fixed.** §5.8 is not a defect at all — it is what turned up while looking for
something that can carry the weight §5.5 dropped.

§5.1–§5.6 are the original pass. §5.7 and §5.8 come from a later pass that went
back with one question — *if live regions are inert, what else can speak?* — and
drove four focus-target shapes and four progressbar runs to answer it.

### 5.1 Starting or leaving a build dropped focus on `<body>`

**Found by:** pressing Enter on "Build the bundle" and asking what had focus
2.5 seconds later.

```
SNAP[building]: {"focus":"<<BODY>>"}
```

`build.js` has two panes in one screen — the setup form and the run view — and
swapped them with `this.els.setup.hidden = true`. The focused element, "Build
the bundle", was inside that subtree. Hiding a subtree that contains the
focused element blurs it, and there is nowhere for focus to go but `<body>`. So
the operator pressed Enter on the primary action of the whole application, and:
their next Tab started again at the top of the document; there was no screen,
no region and no heading in the accessibility tree at the focus point; and a
screen reader was told nothing at all about the build having started. Coming
back with "Change the options" did the same thing in reverse.

This is §10 P1-6 — "focus must never land on `<body>`" — recurring in a place
the second-round rule did not look. The second round found it in a dialog and
wrote the rule about dialogs. **A step-based flow creates the same failure with
no dialog involved: a pane swap *is* a step transition.**

**Fixed** in `build.js`: a `swapPane(show, hide, heading)` helper that hides,
shows, and — only if focus was inside the pane being hidden — moves focus to
the incoming pane's `<h1>`, which now carries `tabindex="-1"` and the new
`.df-focus-target` class. The heading is the right target rather than the pane
wrapper because it *names the phase the operator has just moved into*, which is
also what §10 P2-12 asks a step transition to do.

Re-driven:

```
SNAP[after-start]: focus = h1.df-focus-target tabindex=-1 "Building the bundle"
                   focus-visible  outline=2px solid rgb(22, 94, 181)
SNAP[after-back]:  focus = h1.df-focus-target tabindex=-1 "Build the bundle"
```

### 5.2 `<details>`/`<summary>` announces as a bare phrase on WebKit2GTK

**Found by:** Orca, reading the readiness table.

```
SAID: 'Details for debark command-line tool.'
```

That is all it said. No role, no "collapsed", no "expanded" when it opened. The
AT-SPI tree explains why:

```
unknown "" {tag=details}
  unknown "Details for debark command-line tool" [focusable …] {tag=summary}
```

**WebKit2GTK 2.52.6 maps both `<details>` and `<summary>` to AT-SPI role
`unknown`**, with no EXPANDED and no EXPANDABLE state. The markup is correct
HTML and the design system's §6.12 example is correct; the engine's mapping is
the gap. There are fourteen of these drawers across eight files — every error
detail, the raw event log, the readiness rows, the export verification notes.

Three markups were driven side by side, Orca reading each:

| summary markup | what Orca said |
|---|---|
| plain `<summary>` | "A plain summary." |
| `<summary aria-expanded="false">` | "B summary with aria-expanded." |
| `<summary role="button" aria-expanded="false">` | "C summary role=button **collapsed push button**." |

`aria-expanded` on its own does nothing — the `unknown` role swallows it. Only
the explicit role reaches Orca, and once the role is there the state comes with
it.

**Fixed** with a new module, `frontend/src/shell/disclosure.js`, exporting
`bindDisclosure(details)`: it sets `role="button"` on the summary and an
`aria-expanded` that tracks `open` from the `toggle` event — which is the one
path both the engine's own click handling and a programmatic `open = true` go
through. All fourteen sites call it; `gallery.html` carries the markup and a
three-line listener so the reference page does not go stale. Verified on all
eight harnesses: every `summary.df-summary` present at load reports
`role=button expanded=false`, and no console errors anywhere.

Re-driven on the **shipping binary**, rebuilt from the committed tree with
`hack/copyfrontend` and `-tags "desktop,production,webkit2_41"`, so this is
what the product does and not what the source says it should:

```
before: SAID: 'Details for debark command-line tool.'
after : SAID: 'Details for debark command-line tool collapsed push button.'
```

Chromium/WebView2 maps `<summary>` to a disclosure triangle and announces the
state unaided, so the role swap trades one correct announcement for another
there. Documented in the design README §6.12 with the table above.

### 5.3 `aria-current="step"` is correct, reaches AT-SPI, and is never spoken

**Found by:** standing on the target step and listening.

```
SAID: '1 Target push button.'
```

The attribute is on the button. It reaches the platform: the accessible carries
`{current=page tag=button}` in its AT-SPI attribute set. **Orca 46.1 does not
say it.** So the one thing a gated stepper exists to tell an operator — where
they are — was the one thing only the picture carried.

This is the finding a tree dump is structurally incapable of producing. A dump
shows `current=page` and looks correct. It is correct. It is also silent.

**Fixed** in `shell.js`, by exactly the argument the design system already makes
one step earlier for `.is-complete`: a class says nothing to assistive
technology, so the completed step carries a visually-hidden ", completed". The
current step now carries a visually-hidden ", current step" for the same reason
one rung up — the attribute stays, because it is right and because WebView2 and
NVDA do announce it, and the sentence is added because the attribute is not
enough on the shipping stack.

Re-driven on the real binary:

```
SAID: '1 Target , current step push button.'
```

The design README §6.19 state table now requires it, with the measurement.

### 5.4 The virtualised list's scroll port had no focus-ring rule

**Found by:** reading back the computed outline after focusing the listbox.

```
focus = div.df-vlist.df-scroller role=listbox tabindex=0
        outline=5px auto rgba(53, 132, 228, 0.8)
```

`.df-vlist` is the catalogue's `role="listbox"` port: one `tabindex="0"`
element standing in for up to 60,000 rows, and the only tab stop the whole
catalogue has. It was absent from all three focus selector groups in
`components.css` section 3, so it fell through to the UA ring. That colour is
Adwaita's blue at 80% alpha. It is not `--color-focus-ring`; it is not one of
the pairs the design README §7 measures; and WebView2 would supply a third
colour again — so the app's focus indicator was engine-defined in the one place
an operator spends the most time.

Every other control on every screen produced `outline=2px solid rgb(22, 94, 181)`,
which is `--color-focus-ring` exactly. Only this one did not.

**Fixed** in `components.css`: `.df-vlist` joins the focus rules, drawn **inset**
because `.df-list` is `overflow: hidden` and clips an outset ring — the same
reason `.df-row` is inset. `.df-focus-target` is added alongside it for §5.1's
headings. No colour was introduced: both use `--color-focus-ring`, which §9
re-measures. Re-driven:

```
focus = div.df-vlist.df-scroller role=listbox  outline=2px solid rgb(22, 94, 181)
```

### 5.5 ARIA live regions are inert under Orca on WebKit2GTK — not fixable here

**Found by:** asking. A probe page with three live regions — polite
`role="status"`, assertive `role="alert"`, and a visible `.df-gate-reason` —
each rewritten by a button press driven from the keyboard:

```
SAID: 'Press me: polite status push button.'      (Enter)  — silence
SAID: 'Press me: assertive alert push button.'    (Enter)  — silence
SAID: 'Press me: visible gate reason push button.'(Enter)  — silence
```

Three regions, three key presses, zero speech. The strings never appear
anywhere in Orca's debug log; it did not consider them and reject them, it
never saw them.

**Re-confirmed on the current tree**, in a later pass, after the design system
had changed underneath this document: the same three regions, the same three key
presses, the same zero speech, and `POLITE SPOKE` / `ASSERTIVE SPOKE` /
`GATE SPOKE` each occur **0 times** in a fresh 260,000-line debug log. Orca
still loads `orca.scripts.toolkits.WebKitGtk.script` for the process. Nothing
about this has moved. §5.8 is what came of looking for something that *does*
speak.

The mechanism is in Orca, not in the app. Orca's log shows it loading
`orca.scripts.toolkits.WebKitGtk.script` for this process. That script inherits
from `orca.scripts.default`. **Live-region support lives entirely in
`orca.scripts.web`** — `liveRegionManager`, `liveregions.py`,
`inferLiveRegions` — and `orca.scripts.web` is used for browsers, not for an
application that happens to embed a `WebKitWebView`. No markup this app can
write changes that.

So this is not a defect in Debark and there is no fix to make here. It has
three consequences that the record has to carry, because two documents make
promises that quietly do not hold on Linux:

1. **The design system's gating idiom (README §6.23) has two halves, and only
   one of them works on Linux.** "Rewrite the live region so it is
   re-announced" is inert. "`aria-describedby` pointing at that region" is what
   actually speaks, because a directly-referenced node is included in the
   accessible-description computation regardless of visibility, and Orca reads
   the description on focus. That is exactly what the build screen's trace in
   §4 shows. **The `aria-describedby` is the load-bearing half on this
   platform, and the live region is the load-bearing half on Windows.** Keep
   both; do not "simplify" either away.
2. **The refusal handler's third action — move focus to the control that will
   unblock it — is the only part of a refusal an Orca user perceives.** Driven
   on the real binary: Enter on the locked Packages step produced
   `SAID: '1 Target push button.'` and nothing else. The operator had already
   heard the reason as the button's description when they arrived on it. That
   is adequate, and it is adequate *because* the description exists.
3. **Step transitions are announced by focus, not by the live region.** The
   shell's `say()` on navigation is silent here. What speaks is that the shell
   moves DOM focus into `.df-app__pane`, and Orca reads the landmark it lands
   in: `SAID: 'landmark Choose a target.'`. That works — and it works only
   because every screen's pane is a named region. §10 P2-10 is therefore not a
   tidiness rule on this platform, it is the step-transition announcement.

**§9 of the second-round record listed "live regions under load" as untested,
worried about an announcement storm during a build.** There is no storm. There is
nothing. On Linux, a build's progress is not announced at all.

### 5.6 One artefact left alone, deliberately

The visually-hidden suffix idiom produces a stray space before its comma,
because the engine joins element children of an accessible name with a space:

```
SAID: 'Readiness , one check is blocking a build push button.'
SAID: '1 Target , current step push button.'
```

It is left as it is. In speech the space before a comma is inaudible — the
comma still produces the pause, which is the whole point of the leading `, `.
Every fix trades a certain inaudible artefact for uncertain punctuation
behaviour at a different verbosity setting, or for a phrasing that reads worse
("Target completed"). It **would** be visible on a braille display, which was
not tested. Recorded rather than churned.

### 5.7 A programmatic focus move onto a heading is silent — and `build.js` moves focus onto a heading

**Found by:** asking what §5.1's fix actually says out loud. §5.1 proved that
focus lands on the right element after a pane swap; it proved that with a DOM
readback, not with a transcript. §11.3 of this document is the rule that if you
cannot point at a transcript, assume it did not speak — applied here to §5.1's
own fix.

Four focus targets were built side by side and focused programmatically from a
click handler, with Orca listening:

| focus target | Orca said |
|---|---|
| `<section tabindex="-1" aria-labelledby>` wrapping an `<h1>` | `'landmark Sierra one heading text.'` |
| `<div tabindex="-1" role="region" aria-label>` | `'landmark Sierra three region name.'` |
| a plain `<button>` | `'Sierra four button name push button.'` |
| **`<h1 tabindex="-1" class="df-focus-target">`** | **nothing** |

The heading is not missing from the tree. Its text appears **33 times** in
Orca's debug log for that run — Orca saw it, queried it around the focus event,
and did not speak it. A `.focus()` on it does move DOM focus: driven separately
without Orca, `document.activeElement.id` was the heading's own id and the
computed outline was `2px rgb(22, 94, 181)`, the measured ring.

So the rule that generalises, and it is now design README §2 rule 13: **a focus
move that is meant to announce something must land on a landmark or a control,
never on a heading.**

**The consequence, and it is a defect.** `frontend/src/screens/build.js`'s
`swapPane()` (around line 2027) moves focus to the incoming pane's `<h1>` —
`els.setupHeading` and `els.runHeading`, both
`<h1 class="df-focus-target" tabindex="-1">`. That is the fourth row of the
table. So on Linux, starting or leaving a build restores focus correctly, which
was the severe half of §5.1 and is genuinely fixed, and **announces nothing** —
the operator presses Enter on "Build the bundle" and hears silence, exactly as
before, though their next Tab is now right.

`build.js` is not this pass's file and is live with another package, so this is
reported rather than fixed. The fix is one element: wrap each pane's contents in
a `<section tabindex="-1" aria-labelledby="…the h1's id…">` — or give the
existing pane wrapper `role="region"` and an `aria-label` — and hand focus to
that instead of to the `<h1>`. The first row of the table is that exact shape,
and it speaks the heading's own text, so the announcement the code intends is
the one it would get.

The shell already does this correctly: `shell.js`'s `paneFor()` gives every
`.df-app__pane` `tabindex="-1"`, `role="region"` and an `aria-label`, which is
why a *step* transition speaks (§5.5.3) while a *pane swap inside a step* does
not. Two mechanisms that look alike in the source, one of which works.

### 5.8 Two channels that do speak, found while looking for a replacement for the one that does not

Live regions are inert (§5.5). The follow-up question was whether the design
system has anything else that can carry changing state. Two answers, both
measured on the real engine.

**A rewritten `aria-describedby` target is re-read, with its new text, on the
next arrival.** The description is not a one-shot read on first focus:

```
== arrive on the control
SAID: 'B1 described button push button.'
SAID: 'Phase resolving.'
== press: rewrite the described element in place, focus never leaves
   (silence)
== Tab away, Shift-Tab back
SAID: 'B1 described button push button.'
SAID: 'Phase fetching 4 of forty.'
```

So a `.df-live` region that a control describes is a **pull** channel on Linux:
silent while you stand still, current whenever you arrive. It costs nothing —
the markup is already what the gating idiom requires (§10 P1-4) — and it means
"the reason went stale" is not a failure mode: the operator re-reads it by
leaving the control and coming back. This is the strongest thing that can be
said for §11.2's rule that `aria-describedby` is not redundant with the live
region.

**A `role="progressbar"` announces its first value change, and then nothing.**
This one is a surprise, and it is *more* than a live region manages:

```
SAID: 'Start the run push button.'
   (Enter — a timer now steps aria-valuenow 0 -> 20 -> 40 -> 60 -> 80 -> 100)
SAID: '20 percent.'
   (…then silence for the remaining four ticks)
```

Four independent runs at 1.5s, 2.5s and 3.0s intervals all did the same thing:
the first change spoken, every later one silent. It is **not** keystroke-gated —
pressing a key between ticks did not unblock the next one, which was the obvious
hypothesis and is wrong. And the announcement happens even though the
progressbar is not the focused element, which is the part a live region cannot
do at all.

Two details a screen author needs:

- **`aria-valuetext` is not spoken.** With
  `aria-valuetext="10 percent, resolving"` set, Orca said `'10 percent.'` — it
  computes the percentage from `aria-valuenow`/`-valuemin`/`-valuemax` itself
  and ignores the text. The phase belongs in the visible label, where it can be
  read, not in `aria-valuetext`, where it cannot.
- On **focus**, a progressbar reads name and value:
  `SAID: 'Build progress 20 percent.'` So its current value is always available
  on demand — the same pull shape as the description above.

**This refines §11.7 rather than overturning it.** "A build's progress is not
announced on Linux at all" was very nearly right and is now exactly right: one
tick is announced, at the start, and then the operator hears nothing for the
rest of the run. A single "20 percent" at the top of a four-minute build is not
progress reporting. What the app can honestly offer is that every part of the
run view is *readable* — by focus, by description, and by Orca's structural
navigation, which turns out to work here (§12).

---

## 6. Keyboard traversal on the real engine

Driven with Tab, Shift-Tab, arrows, Home, End, PageUp, PageDown, Enter, Space
and Escape, through `xdotool` into the real window — trusted events, so
`:focus-visible` matched on every stop that should have had it.

### 6.1 What was covered

| Surface | How | Result |
|---|---|---|
| Real app, readiness | 16 Tab, 4 Shift-Tab, Enter, with Orca | no trap, no dead end, reverse order exact |
| Real app, stepper | Tab through all four states, Enter on a locked step | refuses, moves focus to Target |
| Real app, target step | full walk with Orca | four roving groups, one stop each |
| build.demo.html | ~40 Tab with Orca; separate `drive.py` run for the dialog | see §6.2, §6.3 |
| picker-list.demo.html | cursor driven with arrows/Home/End/Page/Space | §7 |
| picker-tray.demo.html | all three dialogs opened, trapped, escaped | §6.3 |
| all eight harnesses | loaded, checked for console errors | none |

The gap between "focusable in the DOM" and "tab stops" is the roving-tabindex
groups, as in the second round, and it is still correct: the theme switch and
the target screen's architecture / distribution / release / variant chips are
each one tab stop with arrow keys inside. Orca confirms it from the outside — "Architecture
panel. amd64. selected radio button", once, not four times.

`grep` finds no positive `tabindex` anywhere in `frontend/src`. DOM order is
the focus order.

### 6.2 The gated primary action

```
SNAP[on-gated-build]: focus = button.df-btn--primary aria-disabled=true
                      aria-describedby=df-build-setup-status-11lt4ff
                      "Build the bundle" focus-visible outline=2px solid rgb(22,94,181)
   (Enter)
SNAP[after-refusal]:  focus = input#df-build-f-1 .df-input.df-mono
gate region: role=status live=polite
   text="To start: choose an output folder; give a signing key, or choose to
         write an unsigned bundle; accept the redistribution notice."
```

`aria-disabled="true"`, `disabled` false, in the tab order, reachable, refuses,
names the reason, lands the operator on the first control that will unblock it.
Orca announces it as "Build the bundle push button grayed" followed by the
reason. Both gated actions on the screen behave this way; so do all four
stepper steps. §10 P1-4 closed, on the shipping stack.

### 6.3 Dialogs

Four dialogs driven: the build screen's stop-confirmation and the tray's
paste-a-list, add-URL and clear-the-selection dialogs.

```
build   labelledby=df-build-cancel-title -> "Stop this build?"  modal=true
        initial focus = "Keep building"          (the dismissive action)
        8 Tab: still inside the dialog, isBody=false
        Escape: openDialog=false, focus="Stop the build", isBody=false
tray    labelledby=pt-dlg-title-3 -> "Paste a package list"     modal=true
        labelledby=pt-dlg-title-5 -> "Add a vendor .deb by URL" modal=true
        8 Tab: inDialog=true, isBody=false
        Escape: focus back on the trigger, isBody=false
```

Every `aria-labelledby` resolves. Every dialog is `:modal`. The clear-the-
selection confirmation focuses the safe option, not the destructive one.

**One engine difference worth writing down.** The second round recorded that in
Chromium, tabbing past the last control of a modal `<dialog>` puts focus on
`document.body` for exactly one step before wrapping, and told the next reader
not to chase it. **WebKit2GTK does not do that** — eight Tab presses in the
build dialog and eight in the tray's never left the dialog and never touched
`<body>`. The second-round note is still correct about Chromium and does not
generalise.

And the confirmation still closes when its subject is over:

```
dialog open before finish: true
   (build:finished arrives underneath the open dialog)
dialog open after finish: false   focus isBody=false   text="Change the options"
```

§10 P1-5, P1-6 and P1-7, closed on the shipping stack.

---

## 7. The virtualiser, re-driven

`picker-list.demo.html` at 60,000 rows, in the real engine, cursor driven from
the keyboard.

| | |
|---|---|
| rows in the DOM | **39** of 60,000 |
| scroll port | `clientHeight` 808, `scrollHeight` 1,920,000 |
| first row | `aria-setsize=60000 aria-posinset=1` |
| at `End` | `aria-posinset=60000`, `scrollTop` 1,919,192 |
| Space on the cursor row | `aria-selected` false → true |

**The set size reaches AT-SPI**, which is what the second round could only show
reaching UIA:

```
list box "Synthetic catalogue" [focusable enabled sensitive showing visible]
  list item "firefox 5.28.1-3ubuntu3 Mozilla Firefox web browser…"
      desc="firefox — Mozilla Firefox web browser…"
      {posinset=1 setsize=60000 xml-roles=option tag=div id=picker-row-slot-0}
  … 38 more, posinset 2…39, every one setsize=60000
```

**And the recycling fix holds.** Cursor on a rendered row, then the port
scrolled to offset 400,000 — the slot the cursor occupied is now a different
package:

```
after scroll away : aria-activedescendant=null   firstRow posinset=12495
next Down         : aria-activedescendant=picker-row-slot-1 -> "chromium-browser"
```

No active option is honest; a wrong one is not. §10 P0-2 and P0-3, closed on the
shipping stack.

`frontend/index.html` carries `class="df-app-host"` on `#app` and the demo
harnesses now set their own bounded frames, so §10 P0-1 is closed and *visibly*
closed: 39 rows in the DOM at 60,000 is the number that proves it.

A skeleton placeholder occupying a not-yet-loaded index carries
`role="option" aria-busy="true" aria-label="Loading"` and the right
`posinset`/`setsize`, so `End` into unloaded territory announces a position and
"Loading" rather than an empty option.

---

## 8. Reflow, zoom, and the OS media queries

All on the real engine. `set_zoom_level` is the same knob Wails exposes to the
operator, so 2.0 here is the product's own 200%, not a devtools emulation.

### 8.1 Reflow — WCAG 1.4.10

Five screens × eight cases (1400/900/640/500/400/320 CSS px at 100%, plus 1400
and 640 at 200%, the latter being 320 CSS px). The probe reports horizontal
document overflow and names any element whose box leaves the viewport without
sitting inside a scroller of its own.

**Zero product offenders in all forty cases.** `docScrollW` equals the viewport
at every width on build, readiness, export and target. The only element ever
flagged is `div.h__side` on the picker harness — the harness's own 400px-fixed
instrument panel, not product markup.

This is the boxed-list redesign paying off: `.df-boxed-list__row` wraps, and
`.df-boxed-list__control` drops below its label at ≤24ch, so the two second-round
findings (`.df-state__actions` losing its third button, `.df-banner` crushing
its body to ~100px) cannot recur in the shape the screens are now built from.

### 8.2 Zoom to 200% — WCAG 1.4.4

Listed as untested in the second round, with the fixed `--row-height` called
out as the thing most likely to break. It does not break.

| case | viewport | `--row-height` | row box | rendered | rows clipped |
|---|---|---|---|---|---|
| 1400 @ 100% | 1400 | 32px | 32 | 39 | 0/39 |
| 1400 @ 200% | 700 | 32px | 32 | 21 | 0/21 |
| 640 @ 200% | 320 | 32px | 32 | 17 | 0/17 |

Page zoom scales the CSS pixel, so a px-valued row height scales with
everything else in it; no row's content exceeds its box at any zoom, and the
virtualiser simply renders fewer, larger rows. WCAG 1.4.4 accepts zoom as the
mechanism, so this closes the item.

**What is still not tested is the OS font-size preference**, which the design
system deliberately does not follow — it sizes in px on purpose. On this stack
that is a defensible choice because webview zoom is available and uniform; it
is a real limitation for a user who scales system text and expects an
application to follow. Recorded, not closed.

### 8.3 The OS media queries, on the real engine

Driven against `gallery.html` by writing real GTK settings and restarting the
view — not by emulation. The table was **re-taken in full** for this pass, one
setting at a time rather than cumulatively, because the earlier run left
`gtk-enable-animations=0` in place while testing HighContrast and so reported
reduced motion as true on a row that does not cause it.

| GTK setting | `prefers-reduced-motion` | `forced-colors` | `prefers-contrast` | `prefers-color-scheme` |
|---|---|---|---|---|
| defaults | false | none | no-preference | light |
| `gtk-theme-name=Adwaita` | false | none | no-preference | light |
| `gtk-theme-name=Adwaita-dark` | false | none | no-preference | **dark** |
| `gtk-enable-animations=0` | **true** | none | no-preference | light |
| `gtk-theme-name=HighContrast` | false | **none** | **more** | light |
| `gtk-theme-name=HighContrastInverse` | false | none | **more** | **light** |
| `gtk-theme-name=highcontrast` | false | none | no-preference | light |
| `gtk-theme-name=HighContrastFoo` | false | none | no-preference | light |
| `gtk-theme-name=Contrast` | false | none | no-preference | light |
| `gtk-theme-name=NotAThemeAtAll` | false | none | no-preference | light |
| `gtk-application-prefer-dark-theme=1` | false | none | no-preference | **dark** |
| HighContrast + prefer-dark | false | none | **more** | **dark** |

The four `prefers-contrast` arms were all queried, not just `more`, so that an
unparsed query could not masquerade as a false: exactly one arm is true in every
row, which means the engine understands the feature.

Two things in that table are worth more than the row they sit in.

**`prefers-contrast: more` is an exact, case-sensitive test of the GTK theme
*name*, and the theme need not exist.** `HighContrastInverse` flipped it in an
image whose `/usr/share/themes` contains no such theme; `highcontrast`,
`HighContrastFoo`, `Contrast` and `NotAThemeAtAll` did not. So the signal is not
"the desktop looks high contrast", it is "the setting says one of two exact
strings" — and those two strings are precisely what GNOME's high-contrast toggle
writes. For this app that is the good case: the query fires when and only when
the operator asked for it.

**`HighContrastInverse` does *not* set `prefers-color-scheme: dark`.** GNOME's
inverse high-contrast theme is the dark one, and an app that follows
`prefers-color-scheme` — which this one does — stays in its light palette under
it unless `gtk-application-prefer-dark-theme` is also set. That is not something
Debark can fix from CSS, and it is why the `prefers-contrast: more` block has to
work in both themes rather than assuming light. It does: every token it touches
is `light-dark()`, and both arms were read back (§9).

**Reduced motion works, on the real setting**, and matches the second round's
emulated numbers exactly:

| | motion allowed | `gtk-enable-animations=0` |
|---|---|---|
| `.df-spinner` | `df-spin` 1.1s infinite | `none` 0.000001s 1 |
| `.df-progress--indeterminate .df-progress__bar` | `df-indeterminate` 1.1s infinite | `none` 0.000001s 1 |
| `.df-skeleton` | `df-shimmer` 1.54s infinite | `none` 0.000001s 1 |
| `.df-btn` transition | 0.08s | 0.000001s |

Which is why the library's rule that a busy control always carries a text label
is load-bearing: with motion off, a spinner alone says nothing.

**Dark mode follows the desktop**: `--color-surface-1` resolves to
`rgb(24, 27, 32)` and `.df-btn--primary` to `rgb(41, 112, 198)` under
`gtk-application-prefer-dark-theme`, and the same two values come back under
`gtk-theme-name=Adwaita-dark`. Re-measured for this pass and unchanged.

> **Superseded, and worth saying what it used to say.** This paragraph
> previously ended: *"The design system's duplicated dark block works on this
> engine, which is a data point for the open `light-dark()` question."* Both
> halves are now false. `tokens.css` carried the dark palette twice — a
> `@media (prefers-color-scheme: dark)` block and a `:root[data-theme="dark"]`
> block, 62 properties each, kept byte-identical by hand — because
> `light-dark()` needs WebKitGTK 2.44 and Debian stable was assumed to be
> older. The packaging work measured the floor instead of assuming it: the
> oldest engine that can be running where the package installs is Ubuntu 24.04
> at GA, **WebKitGTK 2.44.0-2**, because the t64 transition excludes anything
> older from installing at all. 2.44 resolves `light-dark()`. Commit `d103e9f`
> collapsed the two blocks onto it — 53 colours became
> `light-dark(<light>, <dark>)` on bare `:root`, `color-scheme` became the
> switch, **126 declarations were deleted** — and proved it moved nothing: 870
> token resolutions through a real `WebKitWebView`, 3,627 elements × 14
> properties × 6 theme states, zero differences, plus eight pixel-identical
> photograph pairs of the real binary. So there is no duplicated dark block for
> this document to be a data point about, and the question it was a data point
> for is closed. What this pass can still say about the same engine is the
> paragraph above: dark mode follows the desktop, and it does so through
> `light-dark()` now. Re-verified here in all three theme states, resolved
> *through* the engine rather than read off the declaration — because a custom
> property's computed value is its token stream and `light-dark(#F2F4F6,
> #181B20)` reads back verbatim whether or not the engine can resolve it:
>
> | `data-theme` | `--color-surface-1` | `--color-text-primary` | `--color-focus-ring` |
> |---|---|---|---|
> | absent, light desktop | `rgb(242, 244, 246)` | `rgb(20, 24, 29)` | `rgb(22, 94, 181)` |
> | absent, dark desktop | `rgb(24, 27, 32)` | `rgb(232, 235, 239)` | `rgb(122, 178, 248)` |
> | `"light"` on a dark desktop | `rgb(242, 244, 246)` | `rgb(20, 24, 29)` | `rgb(22, 94, 181)` |
> | `"dark"` on a light desktop | `rgb(24, 27, 32)` | `rgb(232, 235, 239)` | `rgb(122, 178, 248)` |

**`forced-colors: active` never matched — including under GTK's own
HighContrast theme, and including under `HighContrastInverse`.** Re-confirmed
this pass on the current tree: `forced-colors: none` was true in all eleven
rows above, and `forced-color-adjust` came back as an empty computed value, i.e.
the property is not implemented in this engine. Forced colours is a
Windows/WebView2 feature, and the `@media (forced-colors: active)` blocks in
`tokens.css` and `components.css` §30 are dead code on the shipping Linux
engine. They stay — WebView2 is a supported target and implements them — but
both now say in the file which platform they are for, because the previous
reader of that code had no way to know.

**What *does* match is `prefers-contrast: more`, and the design system now
answers it.** That was the open item this pass inherited, and it is closed:
`tokens.css` promotes the only two colours in the palette that sit below the
3:1 non-text floor — `--color-border-subtle` and `--color-brand-border`, both
decorative hairlines exempted under WCAG 1.4.11 — to colours the design README
§7.2 already measures above it, and draws the focus ring 3px instead of 2px.
**No new colour was introduced**, deliberately, so the 292 measured pairs and
the colour-vision solve are untouched. 52 pairs re-measured, 0 failures, worst
3.02:1. §9.1 has the numbers and §11.6 has what it does not do.

Verified painted rather than declared, on both ends of the supported engine
range and then in the shipping binary:

| painted value, light theme | 2.52.6 off | 2.52.6 `more` | 2.44.0 off | 2.44.0 `more` |
|---|---|---|---|---|
| `.df-boxed-list` `border-top-color` | `rgb(220,224,230)` | `rgb(125,133,146)` | `rgb(220,224,230)` | `rgb(125,133,146)` |
| `.df-list` `border-top-color` | `rgb(220,224,230)` | `rgb(125,133,146)` | `rgb(220,224,230)` | `rgb(125,133,146)` |
| `.df-headerbar--brand` `border-bottom-color` | `rgb(220,207,184)` | `rgb(130,118,100)` | `rgb(220,207,184)` | `rgb(130,118,100)` |
| `.df-toolbar__divider` `background-color` | `rgb(220,224,230)` | `rgb(125,133,146)` | `rgb(220,224,230)` | `rgb(125,133,146)` |
| `.df-btn.is-focused` outline | `2px solid rgb(22,94,181)` | `3px solid rgb(22,94,181)` | `2px` | `3px` |

and the dark arm resolved through the engine with `data-theme="dark"` set:
`--color-border-subtle` `rgb(46,51,58)` → `rgb(116,125,139)`,
`--color-brand-border` `rgb(54,44,32)` → `rgb(138,125,106)`, on both engines.

2.44.0 matters because it is the oldest engine that can install the package, so
this is not a feature only new desktops get. And in the **real binary**, rebuilt
from this tree with `hack/copyfrontend` and
`-tags "desktop,production,webkit2_41"`: the readiness screen photographed at
1400×900 with and without `gtk-theme-name=HighContrast` differs in **11,910
pixels** at identical geometry — the separators and the ring, and nothing else.

---

## 9. Contrast, recomputed against the current `tokens.css`

**No colour was changed by this pass either.** Every hex value in `tokens.css`
is the one the design work solved for; nothing here moved a colour, and the
colour-vision solve in the design README §8 therefore still stands unre-run.

> **Superseded, and worth saying what it used to say.** This paragraph
> previously opened: *"`tokens.css` is byte-identical to what the design work
> shipped; `git diff` on it is empty."* That is no longer true, and it stopped
> being true twice.
>
> First, commit `d103e9f` rewrote the file's *form* without touching its
> *values*: 53 colours became `light-dark(<light>, <dark>)` on bare `:root`, the
> two duplicated dark blocks went, and 126 declarations were deleted. The
> numbers in the table below are unaffected — they are arithmetic on hex values,
> and the same hex values are still there, now written once instead of twice.
> The **method** did have to change: the audit script used to read a `:root`
> block and a `[data-theme="dark"]` block, and now reads one arm of each
> `light-dark()` per theme. It was checked against three rows already published
> in the design README §7.2 before any of its output was believed —
> `--color-text-primary`/`--color-surface-1` 16.17 / 14.44,
> `--color-border-default`/`--color-row-active` 3.02 / 3.02,
> `--color-focus-ring`/`--color-brand-active` 4.52 / 6.59 — all exact.
>
> Second, **this pass changed `tokens.css` itself**, adding the
> `prefers-contrast: more` block (§8.3). So the honest form of the old sentence
> is no longer "the file is untouched" but the thing that sentence was reaching
> for: *no colour value has changed, and every pair a change made load-bearing
> has been measured.* The block introduces no colour; it re-points two tokens at
> other tokens already in the file, and widens the focus ring. §9.1 has those
> pairs.
>
> The general point is worth keeping, because it is why the sentence was there:
> a contrast table is only as good as its claim about the file it was computed
> from. When that claim changes, re-state it — do not quietly leave the table.

This pass also *painted* `--color-focus-ring` on two surfaces it was not painted
on before (§5.4), and the palette was re-solved during the redesign, so the
second-round numbers describe a palette that no longer exists. They were recomputed
from the file as it is, with the WCAG 2.x relative-luminance formula, rather
than inherited.

`--color-focus-ring` against every surface it can be drawn on, both themes.
Floor is 3:1 (WCAG 1.4.11).

| surface | light (`#165EB5`) | dark (`#7AB2F8`) |
|---|---|---|
| `--color-surface-0` | 5.18 | 8.53 |
| `--color-surface-1` | 5.77 | 7.85 |
| `--color-surface-2` | 6.36 | 7.09 |
| `--color-surface-3` | 6.36 | 6.30 |
| `--color-surface-inset` | 5.52 | 8.79 |
| `--color-row-hover` | 5.66 | 6.39 |
| `--color-row-active` | 5.17 | 5.71 |
| `--color-row-selected` | 5.38 | 6.72 |
| `--color-brand-surface` | 5.61 | 8.10 |
| `--color-brand-canvas` | 5.95 | 8.61 |
| `--color-brand-hover` | 4.96 | 7.33 |
| `--color-brand-active` | **4.52** | 6.59 |

```
pairs checked: 24   failures: 0   worst case: 4.52:1
```

The worst case is the focus ring on a stepper step being pressed, on brand
chrome, in the light theme — 4.52:1 against a 3:1 floor. The eight non-brand
rows reproduce the second-round table exactly, which is itself the useful finding:
**the redesign added brand surfaces without moving the base ones.** The four
brand rows are new here, because the stepper — the control this round introduced
— is the first focusable thing that lives on brand chrome.

The colour-vision solve in the design README §8 was not re-run, because no
colour changed. If one ever does, that solve has to be redone: it is a numeric
fit against a dichromacy simulation, and an earlier hand-picked palette had
`warning` and `danger` at ΔE 0 under deuteranopia.

### 9.1 The pairs the `prefers-contrast: more` block makes load-bearing

The block re-points two tokens and widens the ring; it declares no colour. So
every pair below is either a row of the design README §7.2 re-read under a floor
it did not previously carry, or — for three of them — a pair nobody had reason
to measure until now. All 52 measurements, both themes, floor 3:1.

**Before**, i.e. what these two hairlines are without the block. These are the
§7.1 "deliberate exclusions", and the exclusion is honest: they are decorative
separators, exempt under WCAG 1.4.11.

| foreground | background | light | dark |
|---|---|---|---|
| `--color-border-subtle` | `--color-surface-0` | 1.08 | 1.47 |
| `--color-border-subtle` | `--color-surface-1` | 1.20 | 1.36 |
| `--color-border-subtle` | `--color-surface-2` | 1.33 | 1.23 |
| `--color-border-subtle` | `--color-surface-3` | 1.33 | 1.09 |
| `--color-border-subtle` | `--color-surface-inset` | 1.15 | 1.52 |
| `--color-border-subtle` | `--color-row-hover` | 1.18 | 1.10 |
| `--color-border-subtle` | `--color-row-active` | 1.08 | 1.01 |
| `--color-border-subtle` | `--color-row-selected` | 1.12 | 1.16 |
| `--color-brand-border` | `--color-brand-surface` | 1.36 | 1.30 |
| `--color-brand-border` | `--color-brand-canvas` | 1.44 | 1.39 |
| `--color-brand-border` | `--color-brand-hover` | 1.20 | 1.18 |
| `--color-brand-border` | `--color-brand-active` | 1.09 | 1.06 |
| `--color-brand-border` | `--color-surface-0` | 1.25 | 1.37 |
| `--color-brand-border` | `--color-surface-1` | 1.39 | 1.26 |

`28 measurements, 14 pairs below 3:1` — which is the point: a separator an
operator cannot see is exactly what someone turning on high contrast is
complaining about.

**After.** `--color-border-subtle` resolves to `--color-border-default` and
`--color-brand-border` to `--color-brand-border-control`.

| foreground | background | light | dark | |
|---|---|---|---|---|
| `--color-border-default` | `--color-surface-0` | **3.03** | **4.51** | |
| `--color-border-default` | `--color-surface-1` | **3.38** | **4.15** | |
| `--color-border-default` | `--color-surface-2` | **3.72** | **3.75** | |
| `--color-border-default` | `--color-surface-3` | **3.72** | **3.33** | |
| `--color-border-default` | `--color-surface-inset` | **3.23** | **4.65** | new |
| `--color-border-default` | `--color-row-hover` | **3.31** | **3.38** | |
| `--color-border-default` | `--color-row-active` | **3.02** | **3.02** | worst |
| `--color-border-default` | `--color-row-selected` | **3.15** | **3.55** | |
| `--color-brand-border-control` | `--color-brand-surface` | **3.92** | **4.43** | |
| `--color-brand-border-control` | `--color-brand-canvas` | **4.16** | **4.71** | |
| `--color-brand-border-control` | `--color-brand-hover` | **3.47** | **4.01** | |
| `--color-brand-border-control` | `--color-brand-active` | **3.16** | **3.60** | |
| `--color-brand-border-control` | `--color-surface-0` | **3.62** | **4.66** | new |
| `--color-brand-border-control` | `--color-surface-1` | **4.03** | **4.29** | new |

Plus the twelve focus-ring pairs in the table above, unchanged in colour and
re-read only because the ring is drawn thicker: worst **4.52:1** light, **5.71:1**
dark.

```
pairs checked: 52   failures: 0   worst case: 3.02:1
```

The three new ones are new for the same kind of reason the brand rows were new
last time — a colour got painted somewhere it had not been painted before. The
headerbar's hairline has two sides and §7.2 had only ever measured it against
brand chrome, not against the content surface it abuts; and a `.df-log` or
`.df-code` sits on `--color-surface-inset` and now has a border with a floor.

**What was deliberately not done, and why it is not a shortfall.** Nothing else
in this palette is below a floor. Every text pair is ≥4.5:1 (worst 5.57:1) and
every control boundary ≥3:1, so a high-contrast block that repainted text or
promoted `--color-border-default` to `--color-border-strong` would be improving
a number that already passes, at the cost of flattening the label/description
hierarchy every boxed-list row is built from, or of introducing colours that
would oblige a re-run of the colour-vision solve. `--border-width-hairline`
stays 1px for a different reason: a border is in the box model, so doubling 31
of them is a layout change and would need the whole five-screen × eight-case
reflow matrix in §8.1 re-run. The outline is not in the box model, which is why
the ring could thicken for free. See §11.6 for what a reviewer might reasonably
still want.

**One consequence, stated rather than hidden.** `--color-border-subtle` is also
the border of a `:disabled` or `aria-disabled` control, so under `more` a gated
control's boundary becomes as visible as an enabled one's. That is correct
here: this system never carried the state in that border — §10 P1-4 bans
`disabled` outright, and the state is carried by `aria-disabled`, a dimmed
label, a padlock glyph and a spoken reason, all of which Orca reads (§4) — and
WCAG 1.4.11 exempts a disabled control from the floor in either direction.

**And a cascade trap that shaped the block, measured rather than assumed.** A
`var()` inside a custom property resolves against the **cascaded** value of what
it references, so `--a: var(--b); --b: var(--c)` in one rule collapses *both* to
`--c`. Driven on WebKit2GTK 2.52.6 with a three-token probe: with the rule off,
`--a`/`--b`/`--c` read back `#111`/`#222`/`#333`; with it on,
`#333`/`#333`/`#333`. That is why the block promotes
`--color-border-subtle` to `--color-border-default` and stops, rather than
promoting `--color-border-default` in the same rule — which would have silently
made both `--color-border-strong` and quietly invalidated the table above.

---

## 10. The second round's §10 list, as a status

The prioritised list written to survive the redesign, worked twice: once by the
pass that wrote §1–§9, and once by a later pass that took the four it left open
and the ones whose "closed" turned out to be scoped.

Twenty items, plus one the second pass had to add.

- **Seventeen closed and verified on WebKit2GTK** — 1, 2, 3, 5, 7, 8, 9, 10, 11,
  13, 14, 15, 16, 17, 18, 19 and, newly, **20**.
- **Three closed but narrower than they read.** Item 4 is correct everywhere it
  was driven and wrong in `picker-tray.js`, found by grep after the fact. Item 6
  is fixed for `<body>` and re-opened one rung up: focus lands somewhere sane and
  is not announced. Item 12 works between screens and not inside one — and for
  the same reason as item 6, which is why they moved together.
- **One new, item 21**, which is the rule those two collapsed into.

Three of those four qualifications are the same defect seen from three angles
(§5.7), and none of them was visible to the review that closed the rows: each
row was closed against a DOM readback or a driven surface, and the thing that
was wrong is what a screen reader *says*.

### P0

| # | Requirement | Status |
|---|---|---|
| 1 | The frame's host element must have a height | **Closed.** `index.html` carries `.df-app-host`; the harnesses mount into bounded frames. Proven by 39 rows in the DOM at 60,000 (§7), not by reading the CSS |
| 2 | Clear `aria-activedescendant` when the cursor leaves the window | **Closed, re-driven.** Scroll to offset 400,000 → `null`; next arrow restores it (§7) |
| 3 | Publish the true `aria-setsize` / `aria-posinset` | **Closed, re-driven.** `setsize=60000` on every row, reaching AT-SPI, not just the DOM (§7) |

### P1

| # | Requirement | Status |
|---|---|---|
| 4 | Never `disabled` on a gated action | **Closed where it was checked, and it was not checked everywhere.** Both build-screen actions and all four stepper steps are correct: Orca says "push button grayed" and then the reason (§4, §6.2), and the live-region half of the idiom is inert on Linux (§5.5). But a later grep found **`picker-tray.js` using native `disabled` on two `.df-btn--primary` dialog actions** — see below. The row's honest status is "closed on the surfaces named in §6.2" |
| 5 | Every dialog needs `aria-labelledby`; initial focus on the dismissive action | **Closed.** Four dialogs driven, every label resolves, every one `:modal`, safe option focused (§6.3) |
| 6 | Focus must never land on `<body>` | **Closed for `<body>`, and re-opened one rung up.** The pane-swap fix holds: focus lands on the incoming pane's heading, not on `<body>` (§5.1), and the dialog paths are re-verified (§6.3). But a heading is a place Orca will not name, so the operator is no longer lost and is still not told (§5.7). The rule's next form is P2-12's: it is not enough for focus to *land* somewhere, it has to land somewhere that speaks |
| 7 | A confirmation closes when its subject is over | **Closed, re-driven** (§6.3) |
| 8 | Reflow at the narrowest container the design allows | **Closed.** Zero product offenders across five screens × eight cases down to 320 CSS px (§8.1) |

### P2

| # | Requirement | Status |
|---|---|---|
| 9 | Visual headings must be headings | **Closed.** Every screen has an `h1` — readiness "Before you start", target, build ×2, export, picker (visually hidden). The second-round carry-over "readiness and picker have no headings" is gone |
| 10 | A `<section>` with no accessible name is not a landmark | **Closed, and now load-bearing.** Every group is a named landmark in the AT-SPI tree and Orca reads each one. On Linux this is *how* a step transition is announced, because live regions are not — §5.5 |
| 11 | No form control named only by a placeholder | **Closed.** Every control on the build and target screens announced a real name; the AT-SPI tree shows `placeholder-text` as a separate attribute from the name, with a `labelled-by` relation present |
| 12 | Announce step transitions | **Closed between screens, open inside one.** The shell's live region is silent on this stack; focus moving into the named pane is what speaks (§5.5.3), and that works because `paneFor()` makes every pane a `role="region"` with a name. A pane swap *inside* a screen does not, because it targets a heading (§5.7) |

### P3 — keep what already works

| # | Requirement | Status |
|---|---|---|
| 13 | Roving tabindex for grouped choices | **Kept, verified.** Four groups on target, one stop each, confirmed from outside by Orca (§4) |
| 14 | Skip link first, visible on focus | **Kept.** `SAID: 'Skip to content link.'` is the first thing Orca reads after the frame |
| 15 | No positive `tabindex` | **Kept.** `grep` finds none in `frontend/src` |
| 16 | `aria-pressed` / `aria-selected` drive the appearance | **Kept.** "System toggle button pressed", "selected radio button" throughout |
| 17 | Never signal state with colour alone | **Kept.** Every stepper state has ≥2 non-hue signals, and now a third that is a sentence (§5.3) |
| 18 | Recompute contrast against the brand palette | **Done** (§9), including four brand-chrome pairs the stepper made necessary |
| 19 | Reduced motion collapses every duration | **Kept, and upgraded from emulated to real** (§8.3) |
| 20 | Forced colours hand the palette to the OS | **Closed, by answering the query the platform actually sends.** `forced-colors` does not exist on this engine and never fires; both blocks now say so in the file and stay for WebView2. `prefers-contrast: more` does fire, and `tokens.css` now answers it — measured, no new colour, 52 pairs, 0 failures (§8.3, §9.1, §11.6) |
| 21 | *(new)* A focus move that is meant to announce must land on a landmark or a control | **Open in `build.js`, correct in `shell.js`.** Four shapes driven; a bare heading is silent (§5.7). Reported to the code that owns `build.js`, not fixed here |

**The P1-4 gap, stated plainly.** `frontend/src/screens/picker-tray.js` sets
`pasteCommit.disabled = true` (line 1653 and five more places) and
`urlCommit.disabled = true` (line 2091 and more) on the "Add packages" and
"Add URL" buttons of the paste-a-list and add-URL dialogs. Both are
`df-btn df-btn--primary`. That is native `disabled`, which is what §10 P1-4 and
design README §2 rule 10 forbid, and the consequence is the one the rule
describes: a keyboard operator in that dialog tabs from the textarea to
"Cancel" and wraps, never meeting the primary action and never being told why it
is unavailable. It is the same shape as the build screen's gated button, which
was fixed to `aria-disabled` and measured working in §6.2.

**Found by grep, not by driving it, and reported rather than fixed** —
`picker-tray.js` belongs to another package. It is recorded here because the row
above said "closed" and a reader would reasonably take that as "closed
everywhere". It was closed on the two surfaces that were driven. Three tray
dialogs were opened and escaped in §6.3, which tested modality and focus return;
nobody looked at what the commit button's `disabled` attribute did to the tab
order inside them.

### Carry-overs the second-round record left in prose

- **`picker-tray.js`'s private `.pt-dialog--fallback` / `.pt-scrim`** —
  still there, still superseded by `.df-dialog--fallback`, still not worth the
  risk: the fallback path is unreachable on both target engines and the three
  tray dialogs were confirmed `:modal` on WebKit2GTK this round (§6.3).
- **No `.df-toast` component** — the component now exists in the design system
  (README §6.20). The shell's injected layout is a separate question and
  belongs to whoever consolidates the app frame.
- **Zoom to 200%** — closed (§8.2).
- **The Linux target is untested** — closed. It is now the *only* target that
  has been tested with a screen reader.
- **Orca's structural navigation may not be offered for this application at
  all** — closed, and the suspicion was wrong. `H`, `D`, `T` and `L` all work
  unmodified in the shipping binary (§12).

---

## 11. New requirements, from the stepper and the boxed-list screens

Same rule as the second-round list: each one is something observed here, on the
shipping stack, not a general checklist. Priority order.

### 11.1 A pane swap inside a screen is a step transition and must move focus

`hidden = true` on a subtree containing the focused element blurs it to
`<body>`. Any screen with two panes — a form and a run view, a chooser and a
result — has this by construction, and it is invisible to review because the
code that hides the pane and the code that had focus are in different
functions. Move focus only when focus was inside the outgoing pane — otherwise a
background event yanks the operator out of whatever they were doing. Measured:
§5.1.

**Amended by §11.8.** The original form of this rule said "move focus to the
incoming pane's heading". That fixes the `<body>` bug, which was the severe
half, and announces nothing: Orca does not name a heading it is handed focus on.
Move focus to a **named region** that contains the heading. §5.7 has the four
shapes.

### 11.2 A gated control needs `aria-describedby`, not only a live region

The design system offers both. On Linux only the description is spoken, because
Orca does not load a live-region-aware script for an embedded `WebKitWebView`
at all (§5.5). On Windows the live region is what re-announces a repeated
refusal. **Neither is redundant; the platform decides which one carries.** A
future simplification that drops `aria-describedby` because "the live region
already says it" makes every gate silent on the shipping platform.

### 11.3 An ARIA state can be correct, reach the platform, and still be silent

`.is-complete` is a class, so the design system already required a
visually-hidden ", completed". `aria-current="step"` is real ARIA, reaches
AT-SPI intact, and is still never spoken by Orca 46.1 (§5.3). The rule that
generalises is not "classes need text" — it is **"if you cannot point at a
transcript where a screen reader said it, assume it did not"**. That is only
answerable by running one.

### 11.4 Native HTML semantics are not automatically enough on WebKit2GTK

`<details>`/`<summary>` maps to AT-SPI role `unknown` with no expanded state
(§5.2). Correct HTML, correct in Chromium, mute on the shipping engine. Before
relying on a native interactive element, check what the *shipping* engine
publishes for it — the AT-SPI tree takes about a minute to dump and the answer
is unambiguous.

### 11.5 Every focusable thing needs a focus-ring selector, control or not

`.df-vlist` is a scroll port with `tabindex="0"`, so it is not obviously a
"control" and it was not in the list. It is the single tab stop for 60,000 rows
and it drew the engine's own ring in an unmeasured colour (§5.4). The rule for
`components.css` section 3 is *focusable*, not *control*.

### 11.6 `forced-colors` is Windows-only; Linux signals `prefers-contrast` — **now answered**

Under GTK's HighContrast theme, `forced-colors: active` stays false and
`prefers-contrast: more` becomes true (§8.3). So the design system's careful
`forced-colors` work — including `.df-row`'s deliberate
`forced-color-adjust: none` so a selected row stays visibly selected — does
nothing on the platform this app ships on. That much is unchanged and is now
written into both files, so the next reader of that code is not misled the way
the last one was.

**The open half is closed.** `tokens.css` carries a `prefers-contrast: more`
block. It does exactly the thing the previous version of this section named as
"the cheap and safe part", and stops there deliberately:

```css
@media (prefers-contrast: more) {
  :root {
    --color-border-subtle: var(--color-border-default);
    --color-brand-border: var(--color-brand-border-control);
    --focus-ring-width: 3px;
  }
}
```

Two colours move, and both move to colours already in the file and already
measured — so no new colour enters the palette, the 292 pairs stand, and the
colour-vision solve does not have to be re-run. 52 pairs re-measured, 0
failures, worst 3.02:1 against a 3:1 floor (§9.1). Verified painted on
WebKitGTK 2.52.6 *and* 2.44.0 — the oldest engine that can install the package —
and then in the real binary, where the readiness screen differs by 11,910 pixels
with the theme on (§8.3).

**What a Linux high-contrast user now gets, stated so nobody over-reads it.**
Every box border, row separator, table rule, disclosure edge and toolbar divider
goes from 1.01–1.52:1 to at least 3.02:1, and the focus ring goes from 2px to
3px. That is the whole change. They do **not** get the OS's own colours, because
`forced-colors` is unimplemented in this engine and nothing in CSS can substitute
for it; they do not get black-on-white; and the selected-row tint is still a
tint. The previous version of this section said "a Linux user who turns on high
contrast gets the ordinary palette", and the accurate replacement is: **they get
the ordinary palette with every edge in it above the non-text contrast floor.**

**Why not more.** Nothing else in the palette is below a floor — worst text pair
5.57:1 against 4.5:1, worst control boundary 3.02:1 against 3:1 — so a bigger
block would be improving numbers that already pass, using colours nobody has
solved, at the cost of re-running §8's dichromacy fit. §9.1 lists the two things
that were considered and declined (repainting text, doubling
`--border-width-hairline`) with the reason for each. A reviewer who thinks a
high-contrast user is owed more than a floor is not obviously wrong, and design
README §10.15 records that as an open judgement rather than settling it here.

### 11.7 A build's progress is not announced on Linux — one tick, then silence

Not a defect in this app, and not fixable in it (§5.5). It is a fact about the
product, and the later pass made it exact rather than overturning it.

- A `role="progressbar"` **does** announce, unlike a live region, and it does so
  even when it is not focused — but only its **first** value change. Four runs,
  three different intervals, same result every time; and it is not keystroke-
  gated, so an operator cannot prod it back into speaking (§5.8).
- `aria-valuetext` is ignored. Orca computes the percentage itself, so a phase
  name put there is never heard (§5.8).
- The run view's heading is not announced either, because focus lands on a
  heading rather than a landmark (§5.7). So the sentence "hears the run view's
  heading" in the previous version of this section was **wrong**, and it was
  wrong because it was inferred from a DOM readback instead of a transcript —
  the exact mistake §11.3 exists to prevent, made by this document.

What is true: an Orca user starts a build, hears at most one percentage, and
then hears nothing until they go and read something. Everything that matters is
*readable* — by Tab, by a control's description (§5.8), and by Orca's structural
navigation, which does work here (§12) — and none of it is *announced*. Worth
knowing before anyone writes "the build reports its progress accessibly" in a
user guide.

### 11.8 A focus move is only an announcement if it lands on a landmark or a control

The design system's answer to "how does a step transition announce itself on a
platform with no live regions" is that focus moves into the pane, and Orca reads
the landmark (§5.5.3, §10 P2-10). That is right, and it is right *only* because
the pane is a landmark. Move focus one element deeper — onto the `<h1>` that
names the phase, which is the obviously correct thing to do and is what
`build.js` does — and Orca says nothing at all (§5.7).

So the rule is not "move focus on a transition". It is **move focus to something
the platform will name**: a `role="region"` or `<section>` with an accessible
name, or a control. A heading is neither, on this engine, however much it looks
like the right target. This is design README §2 rule 13, with the four-shape
measurement behind it.

The generalisation, which is §11.3 again from a different direction: the
question is never "is this markup correct" but "can I point at a transcript
where a screen reader said it". `<h1 tabindex="-1">` is correct markup. It is
also mute.

---

## 12. What remains

In rough order of what would find the most.

1. **Windows: NVDA against the WebView2 build.** The Linux half of the definition-of-done item
   is done; this half has never been attempted successfully. It needs a machine
   where a screen reader can run without disrupting someone's desktop — a local
   console session or a VM, not an RDP session and not the owner's live
   desktop. Two findings here are Linux-specific and should be re-checked
   there: `aria-current` (NVDA does announce it, so the added sentence will be
   heard twice — check whether that reads as redundant) and live regions (which
   should work, and carry the gating idiom's other half).
2. **A human listener.** Everything here is a transcript. Verbosity settings,
   interruption, and whether the result is *comprehensible* rather than merely
   correct are not visible in one.
3. **~~`prefers-contrast: more`~~** — done, §8.3 and §11.6. What remains under
   this heading is a judgement rather than a measurement: whether a
   high-contrast user is owed more than a contrast floor. Design README §10.15.
4. **Braille.** Disabled throughout. §5.6 records one artefact that is
   inaudible in speech and would show there.
5. **~~Orca's structural navigation~~ — driven, and it works.** The suspicion in
   §5.5 that Orca might not offer browse mode for an embedded `WebKitWebView`
   was reasonable and wrong. Driven on the **real binary**, keys pressed
   unmodified into the readiness screen:

   ```
   SAID: 'About Debark push button.'
   (H) SAID: 'Before you start heading level 1.'
   (D) SAID: 'notification.'
   (T) SAID: 'Readiness checks. Each row states what is true, what to do about
             it, and the exact command that would fix it.'
       SAID: 'table with 7 rows 4 columns'
       SAID: 'column header.'  SAID: 'row 1.'  SAID: 'column 1.'
   (L) SAID: 'Wrapping to top.'  SAID: 'List with 4 items.'
   (Tab) SAID: '1 Target push button.'
   ```

   Headings, landmarks, tables and lists are all reachable, the table's caption
   is read before it, and Tab still works afterwards. This matters more than it
   looks: it is the difference between "the run view is readable in principle"
   and "an operator can get to the phase row in one keystroke". It is also the
   only reason §11.7's conclusion is tolerable rather than disqualifying.

   What is still not done is a *systematic* browse-mode survey — four keys on
   one screen is a spot check.
6. **The OS font-size preference** — §8.2.
7. **`picker-search.js`'s category chip groups.** Traversed as part of the
   screen walk in the second round and not exercised key by key here either.
   The target screen's four groups were, and they were correct.

---

## 13. Reproducing this

Nothing is committed for the harness, deliberately: it stubs nothing, so there
is no second fake to keep in step, and the parts that matter are four commands.

Build the derived image — the base image is the third-round shots image:

```dockerfile
FROM debark-shots:go126
RUN apt-get update && apt-get install -y --no-install-recommends \
      at-spi2-core gir1.2-atspi-2.0 python3-gi python3-pyatspi \
      dbus-x11 orca speech-dispatcher xdotool
```

Bring up X, a session bus and the accessibility bus, in this order:

```sh
Xvfb :94 -screen 0 1400x900x24 -nolisten tcp &   # your own display number
openbox --sm-disable &
eval "$(dbus-launch --sh-syntax)"
/usr/libexec/at-spi-bus-launcher --launch-immediately &
/usr/libexec/at-spi2-registryd --use-gnome-session &
export GTK_MODULES=gail:atk-bridge GNOME_ACCESSIBILITY=1
export WEBKIT_DISABLE_COMPOSITING_MODE=1
```

Then:

- **the app** — build with `-tags "desktop,production,webkit2_41"`, run it,
  find the window by title `Debark`, and `xdotool windowactivate` it before
  expecting a paint;
- **a screen** — a ~40-line GTK3 + `WebKit2` 4.1 window with a `WebKitWebView`
  pointed at `http://127.0.0.1:<port>/src/screens/<screen>.demo.html`, served by
  `python3 -m http.server` from `frontend/`. This is the same engine the product
  embeds, which is the entire point;
- **keys** — `xdotool key` into the window. Trusted events, so `:focus-visible`
  behaves and default actions run. `run_javascript` reads state back;
- **the tree** — `python3 -c "import gi; gi.require_version('Atspi','2.0'); …"`,
  walking from `Atspi.get_desktop(0)`. Print `get_role_name`, `get_name`,
  `get_description`, the state set, `get_attributes` and the relation set;
- **Orca** — `orca --replace --debug-file=<path>`, then
  `grep 'SPEECH OUTPUT' <path>`. Write `~/.local/share/orca/user-settings.conf`
  as **JSON** with a `profiles.default` key, and point speech-dispatcher at its
  `dummy` module so no audio device is needed. An INI file there kills Orca
  before it registers with the bus, silently.

Interleave `echo "###MARK <what you are about to do>" >> <debug-file>` with the
keystrokes; the marks land in the log in order and turn 260,000 lines of Orca
debug into a readable transcript.

### Four traps the later pass hit, each of which makes a number wrong rather than a run fail

1. **Count the tab stops, then count them again.** Two of this pass's runs were
   wasted because the key sequence drifted by one stop and the marks then
   described the wrong control. Roving-tabindex groups and `tabindex="-1"`
   elements are exactly where the count goes wrong. Read the transcript back
   against the tab order before believing any mark.
2. **`getComputedStyle` immediately after a style write returns the *old* value
   under `prefers-reduced-motion`.** The library's reduced-motion block sets
   `transition-duration: 0.001ms !important` on `*` — with no
   `transition-property`, which means `all` — so writing `color` on a probe
   element starts a transition and the immediate read gets the start value. It
   read every token back as the *first* token in the list, which looks like a
   broken palette rather than a broken probe. Force a reflow (`void
   el.offsetWidth`) and set `transition: none` on the probe.
3. **A token's declared value is not its resolved value.** `getPropertyValue` on
   a custom property returns its token stream, so `light-dark(#F2F4F6, #181B20)`
   reads back verbatim on an engine that cannot resolve it. Push it through a
   real declaration — `probe.style.color = 'var(--x)'` — and read `color` back.
4. **Query every arm of a media feature, not just the one you want.** An engine
   that does not understand `prefers-contrast` returns false for `more`, which
   is indistinguishable from "understood, and not set". Asking for `more`,
   `less`, `custom` *and* `no-preference` and checking exactly one is true is
   what proves the feature is implemented.

### The probes the later pass added

Small pages, none committed, each answering one question:

- **live regions** — three regions (`role="status"`, `role="alert"`, a visible
  `.df-gate-reason`) each rewritten from a key press. Zero speech, and grep the
  log for the strings to prove Orca never saw them rather than saw and skipped.
- **description as a pull channel** — a button with `aria-describedby` at a
  region whose text a key press rewrites. Arrive, press, Tab away, Shift-Tab
  back; the second arrival reads the new text (§5.8).
- **focus-target shapes** — four targets on one page, each focused from a click
  handler: a named `<section>`, a `role="region"` div, a plain button, and a
  bare `<h1 tabindex="-1">`. The last is silent (§5.7). Grep for the heading's
  own text to show it was in the tree.
- **progressbar** — a `role="progressbar"` stepped by `setInterval`, with
  keystrokes interleaved between ticks to test whether the announcement is
  keystroke-gated. It is not (§5.8).
- **`var()` chaining** — three tokens, `--a: var(--b); --b: var(--c)` in one
  media rule. Both collapse to `--c` (§9.1).
- **media features** — `gallery.html` under one GTK setting at a time, including
  theme names that should *not* trigger anything, which is what established that
  `prefers-contrast` is an exact theme-name test (§8.3).

And the rule, unchanged and re-earned twice: **drive it, do not read it.** Four
of the first pass's five findings came from pressing a key and asking what
happened; the fifth came from asking a screen reader to speak and getting
silence. The later pass's two came from the same place, and one of them
(§5.7) is a correction to a fix *this document had already signed off* — because
that fix was verified with a DOM readback and never with a transcript. A DOM
readback tells you where focus is. Only a transcript tells you whether anybody
was told.
## Desktop simplification: final evidence appendix

### S3/01 fixture behavior and focus

The picker demo journal
records a 60,000-row run on WebKitGTK 2.52.6 / Ubuntu 24.04.4 LTS at
1040×720, light theme, zoom 1 and GDK scale 1. The actual list module was
served from GUI `69b974f615f5d43c7cc488c19eda02e946ebdad7` plus the
served-source manifest,
SHA-256 `4877584901fe5d00b91af14ee3a4e9446f62a3ada21d68cacc730ab2475474e8`.
The capture command uses `--demo picker-list`; its top-level `scenario:
"target"` is a default label, and `gui_diff_sha256: "snapshot"` is not a
patch hash. The manifest identifies the source used. The engine revision and
dirty patch are recorded in each journal, but these GTK fixture runs do not
execute the engine or native bindings.

| Recorded check | Result and limit |
|---|---|
| Demo Space and Enter | Both passed: Space changed selection, Enter requested Details. These were DOM-dispatched events, not native keyboard or Orca input. |
| Demo ARIA and recycled focus | 32 rows checked, `aria-setsize="60000"`, no tabbable rows, active descendant present within the window and removed when scrolled away. Focus remained on the list port after click and recycling. This checks DOM state, not announcement. |
| Demo preparation | All recorded controller checks passed; the run has no JavaScript errors. The backend is simulated. |
| Details close and Space | On the actual Packages screen at 960×640 dark, Enter opened authoritative fixture Details, the named Close control restored list focus and Space then changed selection separately. This supersedes the S2 driver-name failure for this fixture path; it does not verify spoken focus restoration. |
| Selected packages at 125% | Visible Selected opened the dialog at a 960×640 client / 768×512 effective CSS viewport. Recorded Close, Clear and Remove controls were in the viewport; one action bar and no horizontal overflow were reported. Focus trap and screen-reader announcements require native checks. |
| Preparation cancellation and explicit Retry | Cancellation stayed stopped without automatic restart; explicit Retry invoked one preparation. |
| Multi-file addition and paste preview | The fixture chooser supplied two files, each added once. Paste preview separated valid, unknown and rejected inputs. This does not exercise a native file chooser or clipboard. |
| Existing-bundle source and cancelled chooser | Source selection required no target, retained explicit unknown metadata and returned to its opener on cancellation. Chooser results were fixtures. |

Five S3/01 journals are **partial or failed journeys**, retained for diagnosis:

- URL addition and URL Undo stopped because the driver could not find
  `dialog[open] input[type="url"]`. They establish opening the dialog only;
  digest validation, submission and Undo did not run.
- Clear/Undo in light and dark stopped after Clear because the driver found
  no control matching `/^Undo/`. Neither journal called `UndoSelection`.
- Copy cancellation reached an unchecked terminal result, but its visible-text
  assertion failed. The captured DOM contains “Incomplete copy — do not
  install”; this is evidence of a stale wording assertion, not a complete
  passing journey. A corrected rerun must verify the whole cancellation path.

All 18 screen journals report one action bar and no horizontal overflow.
Some scrollable content controls are below the viewport; that is not evidence
they are unreachable. These geometry observations cannot certify native
tab order, focus trapping, speech, system choosers or pixel contrast. The
journals were emitted with pending human-inspection fields; the publication
index must retain the coordinator's separate image review and partial-journey
outcomes rather than silently treating every `ready: true` as a pass.

### S3/02 followup fixtures and computed appearance

The followup uses GUI `9a3331364ef59724d6fb9029c38961893ea07a53` and
content manifest
SHA-256 `73a6c04ae754290d6bc386eeeaf8e279da25e4df2b44dfc7674e39ef6a4b277b`,
on the same WebKitGTK/Ubuntu fixture environment. All eleven records report
ready and no JavaScript errors; the ten screen records also have no fixture
errors. These are new runs, not edits to the failed S3/01 verdicts.

| Followup | Recorded result |
|---|---|
| URL and digest and URL removal/Undo | The original URL and digest reached the fixture unchanged, presentation hid credentials, removal used an opaque key and Undo restored the original values. The credential in this fixture is synthetic. |
| Clear/Undo light and dark | Both clicked visible Clear and Undo, called `UndoSelection` and ended with Selected (3). |
| Copy cancellation | Planning, start, cancel, unchecked result and the visible incomplete warning all passed. |
| Build behavior demo | 43 recorded checks passed, including closed technical disclosures, visible default-key uncertainty, explicit unsigned choice, editable idle lifecycle despite a close error, Cancel's Keep building default, stale-event/status rejection and immutable completed-result copy routing. |

The ten screen journals also contain read-only computed-style observations
from `hack/ui-review/appearance.js`. They cover representative visible text
and control boundaries at the captured state, using a 4.5:1 normal-text floor
and reporting 3:1 control-boundary observations separately. Nine records have
no text failures. Target light reports black computed input color against its
checked blue radio background at 3.302:1; the radio paints an indicator rather
than text, so this is a text-classification limitation in the observer. Its
recorded boundary contrast against the parent is 5.378:1. This finding alone
does not justify changing the palette. Observer commit `c22f674` excludes
indicator-only inputs from text assessment while retaining their boundary
observations; an actual-observer regression preserved real text failures.
The saved S3/02 measurements remain the original pre-fix observations.

All ten observations report system light preference, no contrast preference,
no reduced-motion preference and forced colors inactive. Explicit application
dark theme is independent of the system preference. An empty
`reducedMotionFailures` list in these runs therefore does **not** test reduced
motion. This engine reports `light-dark()`, `:has()` and `:focus-visible`
support; `forced-color-adjust: auto` reports unsupported. These captures do
not certify a minimum WebKit version, high contrast, hover/focus states not
present at capture, native-widget pixels or all text in scrolling content.

### S3/03 contrast preferences and minimum-engine fixtures

The next snapshot is GUI `cdb693765ee5defb044e0b4a9ce6f6aa57335558`,
served-source manifest SHA-256
`63333c9acb44ae29786dd2c593a47f2264551aeeeb4b6b6c9547763bb2d47792`.
The capture index groups its
current-engine and floor-engine records separately. The five WebKitGTK 2.52.6
records have no JavaScript or recorded text-contrast failures. The Target
high-contrast/reduced-motion capture at 960×640 and 125% zoom actually reports
both `prefers-contrast: more` and `prefers-reduced-motion: reduce`; no observed
CSS motion exceeds the probe threshold. This is a scoped check of that rendered
Target state, not every progress indicator or transition.

On WebKitGTK 2.36.0 / Ubuntu 22.04.5, Target light/dark and the dark large-selection
journey passed. This engine reports `light-dark()` unsupported; the explicit
palette fallback supplies the observed colors. Recorded text failures are
empty. The S3/03 light large-selection journey was partial: after removal and
Undo, its final Show more viewport assertion failed. Current-engine light and
dark journeys completed all 320 entries, bounded page reads, middle-item
removal and ordered Undo. A passing dark floor-engine journey does not erase
the failed light one or establish a full minimum-engine accessibility pass.

The S3/04 floor-engine rerun subsequently passed all four records: Target
light/dark and the full 320-entry selection journey in light/dark. It used the
same production source and content-manifest hash. Driver commit
`b8fd7fdab77b71ffa25b9e5465a5db1b1cb044df` waits for selection reads and focus
restoration to settle, then verifies the intended control is focused, enabled,
in the viewport and unobstructed before clicking. All four journals have no
JavaScript or fixture errors; both selection journeys end with the native page
limit assertion passing. The earlier failed attempt remains in S3/03; the new
run establishes the corrected fixture journey on WebKitGTK 2.36.0.

### Current native keyboard and speech evidence

The first observations below are the earlier `S4-final2` checkpoint. The
[later native follow-up](#native-list-and-search-follow-up-2041-utc) records
the repaired ancestry/search behavior and the narrower remaining speech gap.

The coordinator drove the actual Wails application at GUI
`f3fca6679ccf57920d4ceb7adc0cce4fcbf18990`, binary SHA-256
`c0fca9cce947d8b91d1a17a0930867b534ea2003679285a6ffc2ea7fdc70c6b0`.
Engine binary SHA-256 is
`b0128035ee641556203c2c52ad1f781ff46e5e4e58123479baa1577045003655`,
with the engine revision and dirty patch recorded above. This is evidence for
that binary; later palette and diagnostic frontend candidates do not silently
inherit its source identity.

The walkthrough used Ubuntu 24.04.4, GTK 3.24.41, WebKitGTK 2.52.6 and Orca 46.1
inside the isolated container on display `:80`. The main client was 1040×720,
light theme, scale 1. Controls were focused through AT-SPI and activated with
native XTest keys in the activated application. This is native input, but
assistive focus does not prove a complete keyboard-only Tab walk.

The action journal,
extracted speech
and raw Orca log
retain the observations. Speech was read from Orca's `SPEECH OUTPUT` lines;
nobody listened to audio, and braille was disabled. Speech-dispatcher's dummy
module initially tried PulseAudio and failed. The successful retry used ALSA
`null` with an isolated default null PCM. The dummy module can still request
audio for its fallback message; its name alone does not remove that dependency.
See the [Speech Dispatcher configuration reference](https://github.com/brailcom/speechd/blob/master/doc/speech-dispatcher.texi)
and [ALSA null PCM definition](https://github.com/alsa-project/alsa-lib/blob/master/src/conf/alsa.conf).

| Native check | Observed result and limit |
|---|---|
| Main and Add menus | Orca named both menus, their items and their popup openers. Arrow keys changed the announced item; Escape returned to the opener. |
| Stage/result navigation | Spoken landmarks included Choose packages, Bundle ready and Review bundle. Native visible input reached the actual Ubuntu 24.04 minimal catalogue and selected `jq`. This was not a full utility-route traversal. |
| Enter Details and Escape | Enter opened the named `jq` dialog and Orca spoke Close package details. Native Escape restored list focus according to the coordinator's focus check. List/row speech after restoration was not captured. Summary-only metadata was presented honestly for this archive. |
| Space and row navigation | Native selection changes were observed, but the speech groups for list selection, restored selection and row toggle are empty. The arrow-row group contains delayed speech from the prior Clear-search-button focus. These are not spoken-selection passes. |
| Advanced and Command | Orca announced expanded/collapsed disclosures and the Format combo box. The corresponding native key sequences were recorded. |
| Signing and build | The coordinator verified the native signing chooser and consent-based creation of a test-profile key. The signed `jq` build reached Bundle ready after about 19s. This single journey does not cover every unsigned, incomplete or failure path. |
| Close during a job | Orca spoke the native Question alert, the stop-running-operation/incomplete-copy warning and Keep working push button. Return left the application running and the build completed: the safe default was exercised. Native Stop and close and timeout/retry were not established by this transcript. |
| Remove/Clear/Undo, URL/paste/file dialogs, copy cancellation and blockers | Scoped fixture results are recorded above. Native speech and a complete keyboard walk for these paths remain unmeasured. |
| Windows/WebView2/NVDA | Unmeasured. |

**At this earlier checkpoint, package-list speech failed.** The
native parent-chain observations
and repeat
return only the Package catalogue list box, despite an associated application.
Combined with the empty speech groups, this prevents accepting native spoken
row navigation, selection changes or restored row focus for that binary. DOM
ARIA checks, fixture focus tests and a visible selection cannot establish speech.
Some speech arrives in the next extracted action group; use timestamps and the
raw log as well as the group label when interpreting a step.

Native reachability beyond the captured Target states, all affected system
dialogs, screen-reader blockers and the complete close/cancel matrix still
need explicit acceptance on the final source. The captures and backend tests
narrow the remaining work; they do not establish blanket native accessibility
compliance.

The later final native checkpoint used production source `cdb6937`, GUI binary
SHA-256 `e9f60d2fc4b5a8f5f555c9d959f6698fdbc19e34435baa0cd9dc1e861ba6e0d3`,
with the same engine binary. The coordinator completed Target → Ubuntu 24.04
minimal → `jq`, the native signing-key chooser and acknowledgement, and a
signed build in about 16s. Navigating to Target during the active build kept
its edits disabled; returning to Bundle recovered the completed signed result.
The completed-checkpoint close journal
records that Alt+F4 after the signed build with unchanged target/selection
exited the application without a second confirmation. The display closed
before the input driver's command returned, producing an X-connection error;
the process exit is established, but its exact elapsed interval is unavailable.

The active-copy cleanup journal
records native Stop and close during a real copy containing an 8GiB test payload.
The process exited and the destination's `.debark-export-incomplete` marker
remained. This establishes the destructive choice's interrupted-copy outcome;
it does not claim the destination is usable or that native timeout/retry was
exercised.

The final binary was also captured on the native GTK HighContrast session in
light
and dark.
With `GDK_SCALE=2` and WebKit zoom 1, the logical client was 960×640 and the
captured client pixels were 1920×1280. The coordinator inspected readable
controls, an anchored primary action and contained scrolling details. GTK
high contrast and disabled animations were configured; these native images do
not supply a computed media-query or pixel-ratio audit. This is a native scale
and theme check, not an OS font-only scaling or forced-colors test. The
final implementation record
retains the broader native outcomes. These checkpoints do not resolve the
package-list speech gap above.

### Native list and search follow-up, 20:41 UTC

The later native `S4-label` run used GUI binary SHA-256
`479ff464ac2e9e395a7ad3df4394b4f8acc8ceef468dd956263cae6c0908a41e`.
Its production changes include copy status `66f97ff`, list ancestry/cursor
`c48bd61`, and the search-label bytes subsequently committed in `959b0ab`.
The list source hash is
`229f144be5396c4d3028101acce138e4decff696fa7461389edc1f22cae0fdbd`;
the search source hash is
`6e5710f38d32d8611e007b401a514604ce20251265b6adfedca8e67581553bad`.
The runtime remained Ubuntu 24.04.4, GTK 3.24.41, WebKitGTK 2.52.6 and Orca
46.1, with the same engine binary. The activated light client was 1040×720 at
scale 1 on display `:90`; these are actual Wails controls and native input.

| Native observation | Evidence and acceptance |
|---|---|
| Upward list ancestry | The post-Tab parent chain reaches Choose packages, the web document, Debark frame, application and desktop. The former list-only ancestry failure is repaired in this run. |
| Search focus and process survival | At 20:40:53.627084, the raw Orca log speaks the search name, entry role and `jq` value. Its explicit labelled-by relation avoids Search's label-inference path. Before and after process lists retain main PID 44781, network PID 44805 and WebKit web-process PID 44807. This log contains no hung/app-gone errors. |
| Native Tab sequence | The action journal and raw speech record Clear search, Filter, Add, Selected, Details, and the list's name/instructions. This is a checked segment of Tab order, not a complete application walk. |
| Initial row entry and selection | Still partial: list entry at 20:41:00.198513 and Down at 20:41:02.191160 are ignored because Orca sees neither the list nor active child as focused. Space changes selection, but its event at 20:41:03.289408 is rejected as not being for Orca's current focus. The list title being spoken does not establish row speech. |
| Details and restored row | Enter speaks the `jqp` dialog at 20:41:04.311545; Escape speaks the correct `jqp` row at 20:41:05.315382; Space then speaks “unselected” at 20:41:06.354025. This sequence passes after the dialog round trip. |
| Multiple selected rows on return | In S4-reveal3's raw log, Escape first speaks selected `jq` at 20:33:24.922408, then the refreshed active descendant corrects it to `jqp` at 20:33:24.966288, about 44ms later. Subsequent Space speaks “unselected”. Cursor identity recovers; the brief first-selected announcement is retained as an observed limitation. |

`S4-reveal3` used GUI binary
`4f5687f5762a14976c477c4407fabe207b3a6635d5b2a6c24b8be9c239c4bb6a`
with the same list bytes and the earlier search field. Its later Search-focus
attempt lost accessibility connectivity; it is not a Search pass. The explicit
label in `S4-label` avoided that failure in the recorded sequence, without
proving the exact upstream cause of the earlier web-process loss.

The remaining package-list gap is **initial spoken cursor/selection before a
Details round trip**, rather than the earlier global ancestry failure. No
complete screen-reader pass is claimed. The original logs remain historical;
raw event and speech timestamps take precedence over extracted transcript
headings because file buffering can place speech in a later action group.
