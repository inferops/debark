# debark-gui design system

> The capture journals, screenshots and raw logs cited in this document are
> part of the project's internal records and are not published with the
> source. The measurements they back are reproduced here in full.

The token layer and component library for the debark desktop GUI. Written so
that a screen author never has to open the CSS to use it correctly.

**Desktop UX contract (2026-09-07):** The shell has one compact toolbar with
Target / Packages / Bundle and a Main menu. Build and copy share Bundle;
System check is a utility. There is no shell footer or shell Back button.
The layout below supersedes older frame examples later in this reference.

```html
<section class="df-screen-layout">
  <div class="df-screen-content">
    <div class="df-app__content"><!-- scrollable screen content --></div>
  </div>
  <div class="df-actionbar"><!-- one screen-owned action area --></div>
</section>
```

The shell pane provides bounded height. Content scrolls inside
`.df-screen-content`; its sibling action bar stays reachable. Use
`.df-actionbar__back` to place a subdued Back at the start and a dominant next
action at the end. `.df-disclosure` styles technical details: continue calling
`bindDisclosure(details)` so expanded state is announced on WebKitGTK.
`.df-menu-anchor` contains a `.df-menu` with `.df-menu__item` buttons; owners
must implement accessible naming, keyboard movement, Escape, outside click,
and focus restoration, as the shell's Main menu does.

Picker results use `.df-picker-workspace` and `.df-picker-results`. Selection
uses a 260px `.df-selection-pane` at **1024 effective CSS pixels or wider**.
Below that width use a named `.df-selection-dialog` and move the same pane
into it; do not create duplicate selection state. Import
`observeSelectionLayout(callback)` from `../shell/layout.js`, keep its returned
unsubscribe, and set workspace `.is-selection-dialog` when callback is false.
This also handles text enlargement relative to the measured 14px body font.
The package row contract remains **32px**. Default/minimum visual fit still
requires the linked foundation and integrated screenshot checkpoints.

```text
frontend/src/design/
  tokens.css       every value the app may use
  components.css   the component library (requires tokens.css first)
  gallery.html     every component in every state, both themes — open it in a browser
  README.md        this file
```

**Load order is not optional.** `components.css` reads tokens defined in
`tokens.css`; loading it first yields an unstyled page.

```html
<link rel="stylesheet" href="design/tokens.css">
<link rel="stylesheet" href="design/components.css">
```

To review the system, open `gallery.html` directly from disk. It makes no
network request, loads no font and no icon library, and its only script is the
theme toggle.

---

## 1. Hard constraints

These are product constraints from the GUI brief, not preferences.

| Constraint | Status |
|---|---|
| Zero npm dependencies, zero build step | No preprocessor, no PostCSS, no Tailwind. The CSS works as authored. |
| No network reference of any kind | No `@import` from a URL, no CDN, no webfont, no remote image, no icon font. System fonts only. |
| Icons | Inline SVG, authored per screen. The library styles the slot; it ships no icons. `gallery.html` shows the expected markup. |
| Engine target | The system WebKit2GTK on Linux (the shipping target) and WebView2 on Windows. |

### Why the CSS looks old-fashioned

No CSS nesting, no `@layer`, no `:has()`. Flat CSS keeps shared screen work easy
to inspect. `tokens.css` uses `light-dark()` on engines that support it and
explicit matching palettes under `@supports not` on older engines.

Desktop UX compatibility captures include **WebKitGTK 2.36.0** as well as
**2.52.6**. The older engine accepts `light-dark()` text inside a custom property
but cannot consume it as a color, which originally erased control boundaries
and made the primary action white on white. The explicit fallback fixes that
failure. This webview compatibility coverage does not change the package's
distribution/dependency support policy; §3 distinguishes the measurements.

---

## 2. The rules a screen author must follow

Thirteen rules. Everything else is detail.

1. **Never hard-code a colour, size, radius or duration.** If you need a value
   that does not exist as a token, that is a gap in this file — raise it, do not
   inline it.
2. **Never signal state with colour alone.** Every status needs an icon shape
   *or* a text label as well; preferably both. About 8% of men cannot separate
   your green from your red.
3. **Never remove a focus ring.** `outline: none` without a replacement is a
   defect. The library already handles this; do not override it.
4. **Monospace is mandatory** for package names, versions, architectures, paths,
   digests and log output. Use `.df-mono` or a component that already applies
   it. A version number in the UI font is a bug.
5. **`--row-height` is a contract with the virtualiser.** Do not add padding,
   margins or borders to `.df-row` that change its box height.
6. **Use `--color-<role>` for icons and rules, `--color-<role>-text` for text.**
   They differ, and swapping them is the most likely way to break contrast.
7. **Every empty, loading and error state names what happened and what to do
   next.** "No results" with no next step is a defect.
8. **Hide things with `el.hidden`, never `style.display`.** The library makes
   `[hidden]` win over every component's own `display`. Setting `display`
   inline destroys the component's layout mode and you then have to remember
   what it was. See §6.17.
9. **Group with `.df-boxed-list`, never with a stack of `.df-panel` cards.**
   The title goes *above* the box as plain text; the box is one seamless
   surface; groups are separated by 32px of space, not by more borders. A
   bordered card on a differently-shaded canvas, used as the default way to
   group anything, is the loudest reason this app read as a web dashboard. See
   §6.18. `.df-panel` is now reserved for something genuinely *floating* over
   content — the tray, a popover's body, a fallback dialog.
10. **Never `disabled` on a gated action.** A disabled control leaves the tab
    order, so the operator never meets it and is never told why it is
    unavailable. Use `aria-disabled="true"` plus `aria-describedby` pointing at
    a `.df-live` or `.df-gate-reason`, and refuse the click in the handler. See
    §6.23.
11. **Layer, do not inline.** Anything transient that appears above the
    catalogue is a `.df-popover`, never an inline `<details>`: an inline
    disclosure above a 60,000-row virtualised list reflows the content below
    it, moving the list's scroll position and keyboard focus. See §6.21.
12. **Brand colours only inside `.df-brand`.** `--color-accent` stays the
    measured, table-safe interactive colour and must never become brand amber;
    brand hues and semantic hues never share a screen region. §8.2 has the
    numbers that make this non-negotiable.
13. **A focus move that is meant to *announce* something must land on a
    landmark or a control — never on a heading.** Moving focus is the only
    push channel this app has on Linux, because ARIA live regions are inert
    there (§6.23), so where focus lands decides whether the operator is told
    anything at all. Measured in the third round, four shapes driven side by
    side with Orca 46.1 on WebKit2GTK 2.52.6, focus moved programmatically
    from a click handler:

    | focus target | Orca said |
    |---|---|
    | `<section tabindex="-1" aria-labelledby>` wrapping the `<h1>` | "landmark *…the heading's text…*" |
    | `<div tabindex="-1" role="region" aria-label>` | "landmark *…the label…*" |
    | a plain `<button>` | "*…name…* push button" |
    | **`<h1 tabindex="-1">` on its own** | **nothing** |

    The heading is not missing from the tree — its text appears 33 times in
    Orca's debug log for that run, so Orca saw it, queried it and declined to
    speak it. `.df-focus-target` styles a focus ring; it does not make
    something announceable. §6.24 and `docs/accessibility.md` §5.7.

---

## 3. Theming contract

- **The modern palette is defined on bare `:root`.** A color that differs
  between themes uses `light-dark(<light>, <dark>)`.
- **The compatibility block repeats those exact values.** All 53 themed
  colors have explicit light and dark fallbacks under
  `@supports not (color: light-dark(#FFFFFF, #000000))`. Keep both dark blocks
  identical and every fallback value equal to its corresponding modern arm.
- **`color-scheme` follows the system or the explicit choice.** The fallback
  mirrors this policy with a dark media query and an explicit dark selector.
  `:where()` keeps theme-selector specificity equal to `:root`, so later
  increased-contrast and forced-color overrides still win.

```css
:root                    { color-scheme: light dark;   /* the system decides */
                           --color-surface-1: light-dark(#F2F4F6, #181B20); … }
:root:where([data-theme="light"]) { color-scheme: light; }
:root:where([data-theme="dark"])  { color-scheme: dark; … }
/* tokens.css also supplies the explicit @supports-not palette. */
```

| `data-theme` on `<html>` | `color-scheme` | Result |
|---|---|---|
| absent | `light dark` | follow the system |
| `"dark"` | `dark` | dark, even on a light system |
| `"light"` | `light` | light, even on a dark system |

To switch themes, set or remove the attribute on the root element:

```js
document.documentElement.setAttribute('data-theme', 'dark');   // force dark
document.documentElement.setAttribute('data-theme', 'light');  // force light
document.documentElement.removeAttribute('data-theme');        // follow system
```

Setting `color-scheme` is not bookkeeping: it is what `light-dark()` reads, and
it is also what makes native scrollbars, form controls and the webview's own
background follow the theme. `color-scheme` inherits, so a token written with
`light-dark()` resolves the same wherever it is used — including where
`.df-brand` re-points `--color-surface-*` at their brand equivalents.

**Elevation uses `--shadow-1`, `--shadow-2` and `--shadow-3`.**
`light-dark()` switches colours, not lengths, and the dark elevation set changes
blur and layer geometry as well as colour, so it cannot express them. Their dark
values are `--shadow-1-dark`…`-3-dark`, declared once on `:root` beside the
light ones; the two dark rules only re-point `--shadow-1..3` at them and carry
no values of their own. Screens use `--shadow-1..3` as before and never the
`-dark` names.

### Measured engine coverage and the compatibility fallback

The earlier packaging review measured the Ubuntu 24.04 package's dependency
floor separately from CSS support: the declared
`libwebkit2gtk-4.1-0 (>= 2.39.91)` is a symbol floor, while the recorded
Ubuntu 24.04 GA engine was 2.44.0-2. Those historical feature probes were:

| Engine | `light-dark()` | nesting | `@layer` | `:has()` |
|---|---|---|---|---|
| 2.36.0 (22.04 GA) | no | no | yes | yes |
| 2.44.0 (24.04 GA package baseline) | yes | yes | yes | yes |
| 2.52.6 (24.04 review environment) | yes | yes | yes | yes |
| 2.52.6 (Debian 13) | yes | yes | yes | yes |

The package review recorded Ubuntu 22.04 and Debian 12 exclusions due to the
t64 dependencies. Running the frontend fixture in an older available webview
does not establish that the shipping package installs on that distribution.

The desktop UX review then exercised the actual current Target modules on
WebKitGTK 2.36.0. The initial capture showed missing radio/border/status colors
and an unreadable Continue action. `cdb6937` adds explicit fallback palettes;
reading a custom property's token text alone would not have caught the defect.
The repaired captures were visually inspected with the following scope:

| Engine / environment | Repaired capture evidence |
|---|---|
| 2.36.0 / Ubuntu 22.04.5 | Target light 1040×720 and dark 960×640: radio state, boundaries, warnings and Continue render; no JavaScript errors or sampled active-text contrast failures. |
| 2.52.6 / Ubuntu 24.04 | Target light/dark: existing palette preserved; no JavaScript errors or sampled active-text contrast failures. |
| 2.52.6 / GTK HighContrast, reduced motion, 1.25 zoom | Target retains the bounded content/action layout; observations report no sampled active-text or reduced-motion failures. |

These are GTK fixture observations, with current visible-state limits. They
do not replace native Wails, Orca, forced-color, full-gallery or package-install
acceptance. See the [review procedure](../../../docs/dev/ui-review.md) and
outer inspected evidence index
for source manifests, dimensions, media matches and the final status.
Static checks also compared all 53 fallback colors with the original arms,
both dark blocks, all six system/explicit theme combinations and contrast
override ordering; the primary-text and radio-boundary floors were unchanged.

**Historical modern-palette equivalence measurement:** both stylesheets were loaded in a
WebKitGTK 2.52.6 `WebKitWebView` and every one of the 145 tokens was resolved
*through the engine* — pushed through a real declaration and read back from
`getComputedStyle`, because a custom property's computed value is its token
stream and `light-dark(#F2F4F6, #181B20)` reads back verbatim whether or not the
engine can resolve it. 145 tokens × 6 theme states × (plain colour, box-shadow,
`.df-brand`-scoped colour) = **870 resolutions**, plus every computed colour on
every element of `gallery.html` (3,627 elements × 14 properties × 6 states).
**Zero differences.** The real binary was then built before and after and
photographed in all three theme states on a light desktop and a dark one: eight
pairs, **pixel-identical** apart from one glyph of the readiness timing text.

---

## 4. Token reference

### 4.1 Surfaces

Elevation runs 0 (furthest back) to 3 (floating). Controls sit on surface 1, 2
or 3; surface 0 is window chrome behind them.

| Token | What it is for |
|---|---|
| `--color-surface-0` | Window chrome / sunken well behind the working area |
| `--color-surface-1` | The main canvas a screen paints on |
| `--color-surface-2` | Panels, cards, list rows, toolbars — anything that reads as "a thing" |
| `--color-surface-3` | Floating: dialogs, menus, popovers |
| `--color-surface-inset` | Recessed: log output, the NDJSON drawer, code and digest blocks |
| `--color-row-hover` | List row under the pointer |
| `--color-row-active` | List row being pressed |
| `--color-row-selected` | Selected list row |
| `--color-backdrop` | Dialog backdrop |

#### Brand surfaces — identity only

Warm sand, and **quarantined**: these appear on the headerbar, the logo mark and
wordmark, and the about surface. That is the entire list. `.df-brand` re-points
`--color-surface-*`, `--color-row-*`, `--color-border-subtle`,
`--color-border-default` and the text tokens at their brand equivalents, so every
component inside it warms up without any component knowing that brand exists.

| Token | What it is for |
|---|---|
| `--color-brand-surface` | Headerbar and window chrome |
| `--color-brand-canvas` | The about / splash ground |
| `--color-brand-hover` / `-active` | Ghost-control hover and press on brand chrome |
| `--color-brand-border` | Chrome hairline. **Decorative**, like `--color-border-subtle` |
| `--color-brand-border-control` | A control's boundary on brand chrome. Meets 3:1 on every brand ground |
| `--color-brand-text` / `-text-muted` | Text on brand chrome |
| `--color-brand-accent` | Identity accent, **about surface only**. Not a substitute for `--color-accent` |
| `--color-brand-amber` / `-clay` / `-oasis` | Fixed in **both** themes. Decorative fills and the logo crate only |

**`--color-accent` must never become brand amber.** The accent carries the
selected-row tint, the focus ring, the checked box, the primary fill, badges and
the progress fill, constantly, on working surfaces. Brand amber is 1.96:1 on the
light brand ground, which is why it is quarantined as decorative wherever this
brand is applied; the identity accent that does carry meaning is the desaturated
brown `--color-brand-accent`, at 4.84:1 / 5.12:1. And brand amber against
`--color-warning` is ΔE 12 — the same colour to a reader. §8.2.

### 4.2 Text

| Token | What it is for |
|---|---|
| `--color-text-primary` | Body copy, package names, headings. The default |
| `--color-text-secondary` | Summaries, versions, captions, help text |
| `--color-text-disabled` | Disabled control labels **only**. Never for text a user must read |
| `--color-text-on-fill` | Text placed on a solid fill |

### 4.3 Borders

| Token | What it is for |
|---|---|
| `--color-border-subtle` | Hairline separators between rows and sections. **Decorative only** — deliberately below 3:1, and never the sole marker of a control's boundary |
| `--color-border-default` | The boundary of an interactive control. Meets 3:1 on every surface |
| `--color-border-strong` | Emphasised boundary: hovered control, active tab underline, table rule |
| `--color-border-focus` | The focus ring colour |

`--color-border-subtle` is the one deliberate exception to the 3:1 rule. WCAG
1.4.11 requires 3:1 only for boundaries needed to *identify* a control; a
decorative row separator is exempt. Do not press it into service as a control
boundary.

### 4.4 Semantic roles

Five roles — `accent`, `info`, `success`, `warning`, `danger` — each exposing
the **same five tokens**. Learn the quintet once:

| Token | What it is for | Floor |
|---|---|---|
| `--color-<role>` | icon / rule / border / meter fill | ≥ 3:1 |
| `--color-<role>-text` | text in that role | ≥ 4.5:1 |
| `--color-<role>-bg` | quiet tinted background (banners, badges) | — |
| `--color-<role>-fill` | solid fill for a filled button or badge | — |
| `--color-<role>-on-fill` | text/icon **on** that solid fill | ≥ 4.5:1 |

`accent` and `danger` additionally have `--color-<role>-fill-hover` and
`--color-<role>-fill-active`, because they are the only roles that appear as
filled buttons.

What each role *means*:

- **accent** — the single interactive colour: primary action, link, selection,
  focus ring. Blue is reserved for "you can act on this"; do not use it
  decoratively.
- **info** — a neutral notice. "Here is a fact you should know."
- **success** — a check passed, a bundle verified, a copy completed.
- **warning** — it will work, but not the way you might assume. (The
  snap-vs-deb caveat; "a base is an assumption, not a measurement".)
- **danger** — it failed, or it destroys something. Never decorative.

### 4.5 Type

Sizes are **px, not rem**, deliberately: the virtualised list depends on a fixed
`--row-height`, and text that scales independently of its box overflows the row.
Ctrl+= zoom in the webview scales everything together, so zoom still works.

| Token | Size | What it is for |
|---|---|---|
| `--font-size-xl` | 19px | screen title — the top of the scale |
| `--font-size-lg` | 16px | section heading, dialog title |
| `--font-size-base` | 14px | body default |
| `--font-size-md` | 13px | control labels, table cells |
| `--font-size-sm` | 12px | secondary row text, help text, log output |
| `--font-size-xs` | 11px | chip text, dense metadata |
| `--font-size-2xs` | 10px | badge counts |

**19px is the top of the scale, deliberately.** A 23px and a 28px step used to
live above it, referenced by nothing but `gallery.html`. They were an invitation
to put a marketing headline inside a tool window, which is half of why the app
read as a web page. Deleted by the brand pass. If a splash or about surface genuinely
wants poster type, it is a one-off on that surface, not a token every screen can
reach for.

Also: `--font-weight-regular|medium|semibold`,
`--line-height-tight|snug|normal|loose`,
`--letter-spacing-tight|normal|wide`, and `--letter-spacing-brand` (0.05em) —
which is for the wordmark and nothing else.

**`system-ui` stays first in every UI font stack. No exceptions.** There is no
`--font-family-display` and there must not be one. Putting a brand face ahead of
`system-ui` means that the day that face is installed on somebody's machine the
app silently stops matching every other GNOME application — invisibly to whoever
is testing it, because their machine is the one where it looks "right". Brand
shows in the type through **weight and tracking on the system font**
(`.df-wordmark`). And no webfont may be fetched: this app works offline.

**Font stacks.** `--font-family-ui` resolves to Cantarell on GNOME, Ubuntu on
Ubuntu, DejaVu/Noto on minimal Debian, and Segoe UI Variable / Segoe UI on
Windows. `--font-family-mono` prefers `ui-monospace`, then Cascadia, JetBrains
Mono, Source Code Pro, DejaVu Sans Mono, Liberation Mono, Noto Sans Mono,
Consolas.

Monospace is load-bearing in this app. Column alignment and unambiguous `0/O`
and `1/l/I` are the entire reason. `.df-mono` also disables ligatures, so `!=`
and `->` in log output stay literal.

### 4.6 Spacing

One scale, 4px base: `--space-1` (4px) through `--space-20` (80px), plus
`--space-0` and `--space-px`. Steps are 1,2,3,4,5,6,7,8,10,12,16,20.

### 4.7 Radius, borders, elevation

| Token | Value | What it is for |
|---|---|---|
| `--radius-sm` | 3px | checkbox, badge, chip |
| `--radius-md` | 5px | button, input, card |
| `--radius-lg` | 8px | dialog, panel |
| `--radius-pill` | 999px | progress track |
| `--radius-circle` | 50% | radio, spinner |
| `--border-width-hairline` | 1px | the default for everything |
| `--border-width-thick` | 2px | selected tab, checked control |
| `--border-width-rule` | 3px | banner left rule, selected row rule |
| `--shadow-1` | | resting card / toolbar edge |
| `--shadow-2` | | popover, dropdown, sticky header |
| `--shadow-3` | | dialog |

Shadows differ between themes: dark uses deeper blacks plus a faint light
hairline, because a soft shadow is invisible on a dark ground. That difference
is geometric as well as chromatic, which is why the three elevation tokens are
the one thing `light-dark()` cannot fold into a single declaration — see §3.
Screens use `--shadow-1..3`; `--shadow-1-dark`…`-3-dark` exist only for the
theme rules to point at.

### 4.8 Focus ring

`--focus-ring-width` (2px), `--focus-ring-offset` (2px), `--color-focus-ring`,
`--color-focus-halo`. The halo is the surface colour, drawn between the control
and the ring so the ring stays visible on a fill of a similar colour.

**Every focusable thing needs a selector in section 3 of `components.css`, and
"focusable" includes things that are not controls.** `.df-vlist` — the
virtualised catalogue's `role="listbox"` scroll port, one `tabindex="0"`
element standing in for up to 60,000 rows — had none, and on WebKit2GTK 2.52.6
it therefore drew the UA ring, `outline: 5px auto rgba(53, 132, 228, 0.8)`.
That is Adwaita's blue, not `--color-focus-ring`, and it is not one of the 292
pairs section 7 measures; WebView2 would supply a third colour again. Measured
and fixed in the third round. `.df-focus-target` is the companion class for a
heading a screen hands programmatic focus to when a pane swaps under the
operator.

### 4.9 Motion

`--duration-fast` (80ms, hover/press), `--duration-base` (140ms, disclosure/tab),
`--duration-slow` (240ms, dialog entry), `--duration-indeterminate` (1100ms,
spinner and indeterminate loops). Easings: `--ease-standard`, `--ease-out`,
`--ease-in-out`, `--ease-linear`.

All durations collapse under `prefers-reduced-motion: reduce`, and looping
animations stop outright. **This is why a busy control must always carry a text
label** — with motion off, a spinner alone communicates nothing.

### 4.10 Density

| Token | Value | What it is for |
|---|---|---|
| `--row-height` | **32px** | the virtualised catalogue row — a contract, see §6.5 |
| `--row-height-comfortable` | 44px | readiness table, non-virtualised rows |
| `--skeleton-row-height` | 32px | must equal `--row-height` |
| `--control-height-sm` / `--control-height` / `--control-height-lg` | **26 / 34 / 40px** | controls |
| `--toolbar-height` / `--headerbar-height` / `--actionbar-height` | 44 / 48 / 56px | bars |
| `--icon-size-sm` / `--icon-size` / `--icon-size-lg` / `--icon-size-xl` | 14 / 16 / 20 / 32px | icons |
| `--hit-target-min` | 24px | smallest clickable box |
| `--measure-prose` | 68ch | max width for readable paragraphs |
| `--sidebar-width` / `--tray-width` | 240 / 320px | layout |
| `--toast-width` | 400px | max width of the single bottom-centre toast |

**Control heights follow GTK/Adwaita**, which is what these controls sit beside on
a GNOME desktop. Adwaita's standard button is about 34px including its border at
1×; this file shipped 30px, under that floor, and a too-short control is one of
the things that made the app read as a web form. Raised by the brand pass.
`--toolbar-height` stays 44px — `components.css` tightened `.df-toolbar`'s
vertical padding to `--space-1` so a 34px control still fits.

### 4.11 Z-index

One ordered scale — do not invent values. `--z-base` 0, `--z-sticky` 10,
`--z-dropdown` 100, `--z-overlay` 1000, `--z-modal` 1001, `--z-toast` 1100,
`--z-progress-top` 1200.

`.df-popover` and `.df-stepper__reason` use `--z-dropdown`; there is deliberately
no separate popover level, because a popover and a dropdown are the same
stacking problem.

---

## 5. Naming conventions

```text
.df-block            component root
.df-block__element   a part of it
.df-block--variant   a variant of it
.is-*                a runtime state you toggle from JS
```

Runtime states: `.is-selected`, `.is-loading`, `.is-active`, `.is-error`,
`.is-open`, `.is-disabled`, `.is-indeterminate`.

**Forced-state classes.** `.is-hover`, `.is-pressed` and `.is-focused` render
the same appearance as `:hover`, `:active` and `:focus-visible`. They exist so
`gallery.html` can show every state at once, and so a keyboard-driven list can
mark its active row while real DOM focus stays on the scroll container. They are
presentation only and convey nothing to assistive technology — always pair them
with the matching ARIA state.

---

## 6. Component inventory

Every component's exact expected markup. Where an `aria-*` attribute is shown,
it is required, not decorative.

### 6.1 Button

```html
<button type="button" class="df-btn df-btn--primary">Build bundle</button>
<button type="button" class="df-btn df-btn--secondary">Choose folder…</button>
<button type="button" class="df-btn df-btn--ghost">Cancel</button>
<button type="button" class="df-btn df-btn--danger">Delete key</button>

<!-- with a leading icon -->
<button type="button" class="df-btn df-btn--primary">
  <svg class="df-btn__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor"
       stroke-width="2" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>
  Add packages
</button>

<!-- busy: keep the label, set aria-busy; the icon is swapped for a spinner -->
<button type="button" class="df-btn df-btn--primary is-loading" aria-busy="true">
  Build bundle
</button>

<!-- icon only: an accessible name is mandatory -->
<button type="button" class="df-icon-btn">
  <svg viewBox="0 0 24 24" aria-hidden="true">…</svg>
  <span class="df-visually-hidden">Refresh catalogue</span>
</button>
```

Variants: `--primary`, `--secondary`, `--ghost`, `--danger`.
Sizes: `--sm`, `--lg`, `--block`.
States: default, `:hover`, `:active`, `:focus-visible`, `[disabled]`,
`.is-loading`, selected.

One primary button per screen region. `--danger` is for destruction, never for
"the other option".

**Selected.** A `--secondary` or `--ghost` button standing in for a choice that
is currently in force gets a selected look from its accessible state — tint
**and** weight, never tint alone:

```html
<button type="button" class="df-btn df-btn--secondary" aria-pressed="true">Dark</button>
```

`aria-pressed="true"`, `aria-checked="true"` and `.is-active` all trigger it, so
the visual state cannot drift from the accessible one. Prefer a chip or a tab
for a real choice; this exists so that a button *used* as one is not silently
unreadable. It is scoped to secondary and ghost on purpose: primary and danger
already carry a solid fill that means something, and repainting a pressed danger
button accent would stop it looking dangerous.

### 6.2 Checkbox and radio

```html
<label class="df-check">
  <input type="checkbox" class="df-check__input">
  <span class="df-check__label">Include recommends</span>
</label>

<label class="df-radio">
  <input type="radio" name="arch" class="df-radio__input">
  <span class="df-radio__label">amd64</span>
</label>
```

Indeterminate: set the DOM property, which is what a screen reader announces.

```js
input.indeterminate = true;
```

`.df-check__input.is-indeterminate` mirrors the pseudo-class for static
rendering only (the gallery uses it). It must **accompany** the DOM property,
never replace it.

The indeterminate mark is a **bar**, not a tick — a different shape, so
"some of this group is selected" survives without hue.

### 6.3 Text input and search field

```html
<div class="df-field">
  <label class="df-field__label" for="name">Bundle name</label>
  <input id="name" class="df-input" value="airgap-2026-09">
  <span class="df-field__hint">Written to the output folder as-is.</span>
</div>

<!-- error: aria-invalid AND an icon AND words. Never colour alone. -->
<div class="df-field">
  <label class="df-field__label" for="url">Vendor .deb URL</label>
  <input id="url" class="df-input is-error" aria-invalid="true">
  <span class="df-field__error">
    <svg viewBox="0 0 24 24" aria-hidden="true">…</svg>
    Not a valid URL. Expected http:// or https://.
  </span>
</div>

<!-- The name is required. A placeholder is not a label: it disappears the
     moment anything is typed, and .df-field__label is a <span>, not a
     <label for>. Use aria-label, or a real <label for> on a wrapping field. -->
<div class="df-search">
  <svg class="df-search__icon" viewBox="0 0 24 24" aria-hidden="true">…</svg>
  <input class="df-search__input" type="search" aria-label="Search packages"
         placeholder="Search packages">
  <button type="button" class="df-search__clear" hidden aria-label="Clear search">
    <svg viewBox="0 0 24 24" aria-hidden="true">…</svg>
  </button>
</div>
```

The focus ring is drawn on `.df-search` via `:focus-within`, so the leading icon
sits inside it. Toggle the clear button with the `hidden` attribute.

`textarea.df-input` is monospace by default — it is used for pasted package
lists.

### 6.4 Chip / tag

```html
<!-- filter chip: a real button, toggled with aria-pressed -->
<button type="button" class="df-chip" aria-pressed="true">
  <svg class="df-chip__icon" aria-hidden="true">…</svg> Development
</button>

<!-- selection tray entry -->
<span class="df-chip df-chip--removable">
  <span class="df-chip__label df-mono">nginx-full</span>
  <button type="button" class="df-chip__remove" aria-label="Remove nginx-full">
    <svg viewBox="0 0 24 24" aria-hidden="true">…</svg>
  </button>
</span>
```

Role variants: `--info`, `--success`, `--warning`, `--danger`.

A selected filter changes tint **and** border weight **and** should gain a
leading tick — three signals, only one of which is colour. `.df-chip__icon`
sizes that tick; it exists because until the accessibility pass the library
shipped no class for it and the advice above was not buildable. The remove button
fills a whole `--hit-target-min` (24px) box even though its glyph is 10px.

Both `aria-pressed="true"` and `aria-checked="true"` give the selected look, so
a toggle group and a radio-style group each drive it from their own accessible
state rather than from a parallel class.

### 6.5 List row and the virtualiser

**`--row-height` is a contract.** The virtualiser computes
`topIndex = floor(scrollTop / rowHeight)` and sizes a spacer from it. If the
rendered row box stops matching the token, the list tears while scrolling. Do
not add padding, margin or borders to `.df-row` that change its height, and
never make `--row-height` fractional.

```html
<div class="df-list" style="height:420px">
  <div class="df-vlist" role="listbox" aria-label="Catalogue">   <!-- scroll port -->
    <div class="df-vlist__sizer"  style="height:2252544px">      <!-- total × rowHeight -->
      <div class="df-vlist__window" style="transform:translateY(64000px)">
        <!-- only the visible slice, ~40 rows -->
        <div class="df-row" role="option" aria-selected="false">
          <input type="checkbox" class="df-check__input df-row__check"
                 aria-label="Select nginx-full">
          <span class="df-row__name df-mono">nginx-full</span>
          <span class="df-row__version df-mono">1.24.0-2ubuntu7.1</span>
          <span class="df-row__summary">high performance web server</span>
          <span class="df-row__badge"><span class="df-badge">universe</span></span>
        </div>
      </div>
    </div>
  </div>
</div>
```

Grid columns: checkbox, name (max 22ch), version (max 15ch), summary (flex),
badge slot.

States: `:hover`, `:active`, `.is-selected` / `[aria-selected="true"]`,
`.is-disabled`, `:focus-visible`.

A selected row carries a tint **and** a 3px leading rule **and** a checked box.
The focus ring on a row is drawn **inset**, because the list clips an outset
one.

`.df-row--header` is a sticky category header at the same fixed height.

`.df-row` deliberately has **no transition** — rows are recycled during scroll,
and a transition would animate a row into its new neighbour's state.

### 6.6 Skeleton / loading placeholder

```html
<div class="df-skeleton-row">
  <span class="df-skeleton df-skeleton--check"></span>
  <span class="df-skeleton" style="width:78%"></span>
  <span class="df-skeleton" style="width:64%"></span>
  <span class="df-skeleton" style="width:88%"></span>
  <span class="df-skeleton" style="width:44px"></span>
</div>
```

`--skeleton-row-height` equals `--row-height`, so nothing shifts when real data
arrives.

### 6.7 Progress

```html
<!-- determinate -->
<div class="df-progress-label">
  <span>Downloading Packages indexes</span>
  <span class="df-progress-label__value">18%</span>
</div>
<div class="df-progress" role="progressbar" aria-valuenow="18"
     aria-valuemin="0" aria-valuemax="100" aria-label="Downloading">
  <div class="df-progress__bar" style="width:18%"></div>
</div>

<!-- indeterminate: no aria-valuenow -->
<div class="df-progress df-progress--indeterminate" role="progressbar"
     aria-label="Resolving dependencies">
  <div class="df-progress__bar"></div>
</div>

<!-- thin top-of-screen bar (position: fixed) -->
<div class="df-progress df-progress--top df-progress--indeterminate" role="progressbar">
  <div class="df-progress__bar"></div>
</div>
```

Role variants: `--success`, `--warning`, `--danger`.

**A progress bar without a text label is a defect.** Under reduced motion the
indeterminate bar stops moving, and the label is all that is left.

**And on Linux the label is all a screen-reader user gets after the first
tick.** Measured in the third round with Orca 46.1 on WebKit2GTK 2.52.6, four
separate runs: a `role="progressbar"` whose `aria-valuenow` changes *is* announced —
"20 percent." — even when it is not the focused element, which is more than an
`aria-live` region manages there (§6.23). But only **once**. Every later change
in the same run was silent: 0→20 spoken, then 40, 60, 80 and 100 not, across
runs that stepped at 1.5s, 2.5s and 3s. It is not keystroke-gated either —
pressing a key between ticks did not unblock the next one. So a determinate bar
announces that a run has started and then goes quiet for the rest of it.

Two consequences for a screen author:

- `aria-valuetext` is **not spoken**. Orca computes and reads the percentage
  from `aria-valuenow`/`-valuemin`/`-valuemax` itself: with
  `aria-valuetext="10 percent, resolving"` set, it said "10 percent." The phase
  belongs in the text label, where it can be read, not in `aria-valuetext`.
- The label and the phase row are the *only* durable carriers. Write them so
  that an operator who reads them at any moment learns where the run is, rather
  than assuming the announcement stream did it.

### 6.8 Banner / inline message

```html
<div class="df-banner df-banner--warning" role="status">
  <svg class="df-banner__icon" viewBox="0 0 24 24" aria-hidden="true">…</svg>
  <div class="df-banner__body">
    <p class="df-banner__title">Firefox on Ubuntu is a snap</p>
    <p class="df-banner__text">The <span class="df-mono">firefox</span> deb is a
      transitional package that installs a snap.</p>
  </div>
  <div class="df-banner__actions">
    <button type="button" class="df-btn df-btn--secondary df-btn--sm">Remove it</button>
  </div>
</div>
```

Variants: `--info`, `--success`, `--warning`, `--danger`.
Use `role="alert"` for danger and `role="status"` for the rest.

Each variant expects a **distinct icon shape**: circle-i (info), circle-tick
(success), triangle (warning), octagon-x (danger). The library colours the icon;
you supply the shape.

The banner is a **wrapping flex row**, not a three-column grid. The body claims
a floor of 24ch and the actions drop onto their own line rather than squeezing
it — a banner in the 320px selection tray used to leave the body about 100px
wide, which is a 1.4.10 reflow failure. An empty `.df-banner__actions` is now
`display: none` and costs nothing, so keeping it in the markup is optional
rather than load-bearing.

### 6.9 Status (icon + label)

```html
<span class="df-status df-status--warning">
  <svg viewBox="0 0 24 24" aria-hidden="true">…</svg>
  <span>Degraded</span>
</span>
```

Variants: `--info`, `--success`, `--warning`, `--danger`, `--pending`. This is
the canonical "never colour alone" unit — use it anywhere a state is shown.

### 6.10 Dialog

Uses the native `<dialog>` element with `showModal()`, which provides focus
trapping and Escape-to-close for free.

```html
<dialog class="df-dialog" aria-labelledby="rm-key-title">
  <div class="df-dialog__inner">
    <div class="df-dialog__header">
      <span class="df-dialog__title" id="rm-key-title">Remove the signing key?</span>
      <button type="button" class="df-dialog__close" aria-label="Close">…</button>
    </div>
    <div class="df-dialog__body">…</div>
    <div class="df-dialog__footer">
      <button type="button" class="df-btn df-btn--ghost">Cancel</button>
      <button type="button" class="df-btn df-btn--danger">Remove key</button>
    </div>
  </div>
</dialog>
```

`--wide` widens it.

**`aria-labelledby` is not optional.** The engine gives a `<dialog>` the dialog
role but no name, so without it a screen reader announces "dialog" and stops —
in the one place the operator most needs to know what is being asked. The
accessibility pass found the build screen's stop-confirmation in exactly that
state. Give initial focus to the **dismissive** action, not the destructive
one, and put focus back on the trigger when it closes — via the `close` event,
which is the only path Escape goes through.

Two classes, and they are not interchangeable:

- `.df-dialog-overlay` is a **wrapper**. It paints the backdrop and centres
  whatever is inside it. It is what `gallery.html` uses to show a dialog on a
  static page. Never put it on the `<dialog>`.
- `.df-dialog--fallback` is a **modifier on the `<dialog>` itself**, for an
  engine without `showModal()`. It centres the dialog and paints the backdrop
  with a spread shadow, because `::backdrop` only renders for `showModal()`.

Screens should use `showModal()` and only fall back if it is missing.

### 6.11 Tabs

```html
<div class="df-tabs" role="tablist">
  <button type="button" class="df-tab" role="tab" aria-selected="true">
    Catalogue <span class="df-tab__count">70,412</span>
  </button>
  <button type="button" class="df-tab" role="tab" aria-selected="false">Selected</button>
</div>
<div class="df-tabpanel" role="tabpanel">…</div>
```

Selection is driven by `aria-selected`, so the accessible state and the visual
state cannot drift. A selected tab changes colour **and** weight **and** gains a
2px underline.

### 6.12 Disclosure drawer and log

The raw NDJSON log, collapsed by default. This audience trusts what it can
inspect; hiding the log entirely costs more than it gains.

```html
<details class="df-disclosure">
  <summary class="df-summary">Raw event log</summary>
  <div class="df-disclosure__body">
    <pre class="df-log"><span class="df-log__line">{"event":"build.start"}</span><span
      class="df-log__line df-log__line--warn">{"level":"warn",…}</span></pre>
  </div>
</details>
```

Line variants: `--warn`, `--error`, `--muted`. The caret is drawn in CSS and
rotates on open — a shape change, not a colour change. `.df-log` wraps long
lines rather than scrolling horizontally, because an NDJSON line is long and the
operator is scanning it, not editing it.

**The markup above is correct HTML and it is not enough on the shipping
engine.** WebKit2GTK 2.52.6 maps both `<details>` and `<summary>` to AT-SPI
role `unknown`, with no EXPANDED and no EXPANDABLE state. Orca 46.1 announced
the readiness screen's drawer as *"Details for debark command-line tool."* —
a phrase, with no indication that it is a control or that anything happened
when it was pressed. Three markups were driven side by side on that engine
(third round):

| summary markup | what Orca said |
|---|---|
| plain | "A plain summary." |
| `aria-expanded` only | "B summary with aria-expanded." |
| `role="button"` + `aria-expanded` | "C summary role=button **collapsed push button**." |

`aria-expanded` alone does nothing — the `unknown` role swallows it. So a
consumer must give the `<summary>` an explicit `role="button"` and keep
`aria-expanded` in step with the element's own `open`, which the engine toggles
without touching any attribute. `frontend/src/shell/disclosure.js` exports
`bindDisclosure(details)` for exactly this and every drawer in the app goes
through it. Chromium/WebView2 maps `<summary>` to a disclosure triangle and
announces the state unaided, so the role swap trades one correct announcement
for another there.

### 6.13 Table (readiness checks)

```html
<table class="df-table">
  <thead>
    <tr>
      <th class="df-table__status">Status</th>
      <th class="df-table__label">Check</th>
      <th class="df-table__detail">Detail</th>
      <th class="df-table__action">Action</th>
    </tr>
  </thead>
  <tbody>
    <tr>
      <td class="df-table__status">
        <span class="df-status df-status--warning">
          <svg aria-hidden="true">…</svg><span class="df-visually-hidden">Warning</span>
        </span>
      </td>
      <td class="df-table__label">Disk space</td>
      <td class="df-table__detail">4.2 GiB free; a full noble bundle needs ~6 GiB</td>
      <td class="df-table__action">
        <button type="button" class="df-btn df-btn--secondary df-btn--sm">Change location</button>
      </td>
    </tr>
  </tbody>
</table>
```

An icon-only status cell needs a visually-hidden text label — otherwise the
status is invisible to a screen reader.

**`.df-table--fixed`** now works with more than one label column. The default
column rules are written for `table-layout: auto`, where `width: 1%` means "as
narrow as the content allows"; under fixed layout `1%` is taken literally, the
status and action columns collapse and a `white-space: nowrap` label cannot claim
the width its text needs. So `--fixed` states the status column at 48px and the
action column at 20%, leaves every label and detail column at `auto` — under
fixed layout those share what is left, equally, however many of them there are —
and lets labels and action buttons wrap.

Measured: a five-column fixed table with **two** label columns fits from a
**400px** container. At 320px it still overflows by about 15px, because a button
label has an irreducible minimum width. A readiness table is a main-pane
component, not a 320px-tray one; if one has to go in the tray, wrap it in
`.df-scroller`.

### 6.14 Bars and panels

```html
<div class="df-headerbar">
  <span class="df-headerbar__title">Debark</span>
  <span class="df-headerbar__subtitle df-mono">ubuntu/noble/server/amd64</span>
  <div class="df-spacer"></div>
</div>

<div class="df-toolbar df-toolbar--sticky">
  <div class="df-toolbar__group">…</div>
  <div class="df-toolbar__divider"></div>
  <div class="df-spacer"></div>
</div>

<div class="df-actionbar">
  <span class="df-actionbar__status">412 packages · ~1.8 GiB</span>
  <button type="button" class="df-btn df-btn--ghost">Back</button>
  <button type="button" class="df-btn df-btn--primary">Build bundle</button>
</div>
```

`.df-headerbar` is `min-height`, not `height`, and it **wraps**: a headerbar
carrying a `.df-stepper` has to reflow at a 320px container rather than pushing
the last step off the edge. `--headerbar-height` is its resting height, not a
clamp. `.df-toolbar`'s vertical padding is `--space-1`, because a 34px control
plus `--space-2` top and bottom overflows a 44px bar.

**`.df-panel` is not the grouping primitive.** Use `.df-boxed-list` (§6.18).
`.df-panel` is now reserved for something genuinely *floating* over content — the
selection tray, a sidebar, the container a popover or a fallback dialog is built
from. That is the whole list. `.df-panel--flush` drops its padding;
`.df-panel--raised` puts it on surface-3 with `--shadow-2` for a tray that is
detached from the canvas behind it.

Stacking `.df-panel` cards as the default way to group things is the loudest
reason this app read as an admin dashboard: peripheral vision counts rectangles
before it reads a label, and `screens/build.js` stacks **seven in one column**,
each with its title inside the box. Replacing those with `.df-boxed-list` groups
is the single biggest lever the redesign has.

The app frame — `.df-app-host`, `.df-app`, `.df-app__*` — is §6.24.

### 6.15 Empty, loading and error states

```html
<div class="df-state df-state--error">
  <svg class="df-state__icon" aria-hidden="true">…</svg>
  <span class="df-state__title">Could not reach the archive</span>
  <p class="df-state__text">Check the builder machine's network, or load a
    snapshot file instead.</p>
  <div class="df-state__actions">
    <button type="button" class="df-btn df-btn--primary df-btn--sm">Retry</button>
  </div>
  <div class="df-state__detail">
    <details class="df-disclosure">
      <summary class="df-summary">What failed</summary>
      <div class="df-disclosure__body"><pre class="df-log">…</pre></div>
    </details>
  </div>
</div>
```

Variants: `--loading`, `--error` (the default is the empty state). Headline,
sentence, action. `.df-state__detail` exists so an error can show the raw
failure verbatim — this audience wants it.

### 6.16 Utilities

| Class | What it does |
|---|---|
| `.df-mono` | monospace, ligatures off |
| `.df-visually-hidden` | available to screen readers, invisible on screen |
| `.df-truncate` | single-line ellipsis (needs `min-width: 0` ancestors) |
| `.df-stack` / `--tight` / `--loose` | vertical flex with one gap |
| `.df-cluster` | horizontal wrapping flex |
| `.df-spacer` | flexible gap that pushes siblings apart |
| `.df-scroller` | a scrolling region (needs a bounded parent) |
| `.df-link` | underlined accent link |
| `.df-badge` | small marker; `--info`/`--success`/`--warning`/`--danger`/`--count` |
| `.df-spinner` / `--lg` | busy indicator; **always** pair with a text label |
| `.df-text-primary`, `.df-text-secondary`, `.df-text-sm`, `.df-text-md` | text colour/size shortcuts |
| `.df-live` | a visually-hidden live region that is still a valid `aria-describedby` target — §6.23 |
| `.df-gate-reason` | the same sentence on screen, with an icon slot — §6.23 |

### 6.17 Hiding things

The library sets `[hidden] { display: none !important }` globally. Without it,
any component rule that sets `display` (`.df-stack`, `.df-progress-label`,
`.df-state__actions`, …) beats the user-agent's `[hidden]` rule, and
`el.hidden = true` silently does nothing.

So this works, and is the way to hide anything:

```js
el.hidden = true;   // hide
el.hidden = false;  // show, restoring the component's own display
```

**Do not reach for `style.display`.** Setting it inline overwrites the
component's own layout mode, and showing the element again then needs you to
know whether it was `flex`, `grid` or `block`. `hidden` has no such problem.

The `!important` is deliberate and is one of only two in the library (the other
is the reduced-motion block): the whole point is to beat a more specific
selector, and "this element is not here" has no legitimate override.

### 6.18 Group and boxed list — **the grouping primitive**

This is what replaces a stack of `.df-panel` cards. Read rule 9 first.

```html
<div class="df-groups">                              <!-- 32px between groups -->
  <section class="df-group" aria-labelledby="g-out">
    <h2 class="df-group__title" id="g-out">Output</h2>    <!-- ABOVE the box -->
    <p class="df-group__description">Where the bundle folder is written.</p>

    <div class="df-boxed-list">
      <!-- a field row -->
      <div class="df-boxed-list__row">
        <div class="df-boxed-list__text">
          <label class="df-boxed-list__label" for="bn">Bundle name</label>
          <span class="df-boxed-list__description">Written to the folder as-is.</span>
        </div>
        <div class="df-boxed-list__control df-boxed-list__control--field">
          <input id="bn" class="df-input" value="airgap-2026-09">
        </div>
      </div>

      <!-- a whole-row button: the row element IS the button -->
      <button type="button" class="df-boxed-list__row">
        <span class="df-boxed-list__text">
          <span class="df-boxed-list__label">Choose a signing key…</span>
        </span>
        <svg class="df-boxed-list__chevron" aria-hidden="true">…</svg>
      </button>

      <!-- a selected choice: tint + 3px rule + a checked radio -->
      <label class="df-boxed-list__row is-selected" aria-checked="true">
        <input type="radio" name="target" class="df-radio__input" checked>
        <span class="df-boxed-list__text">
          <span class="df-boxed-list__label">Ubuntu 24.04 LTS</span>
        </span>
      </label>

      <!-- a full-bleed child: a table, a list, a log -->
      <div class="df-boxed-list__slab"><table class="df-table">…</table></div>

      <div class="df-boxed-list__footer">6 rows<div class="df-spacer"></div>…</div>
    </div>
  </section>
</div>
```

| Class | Contract |
|---|---|
| `.df-groups` | vertical flex, 32px gap. `--tight` for 24px |
| `.df-group` | one group: title, optional description, one box |
| `.df-group__title` | **plain text above the box.** Body size, semibold. Use a real `<h2>`/`<h3>` — a step's sections are headings (WCAG 1.3.1) |
| `.df-group__description` | optional sentence under the title, secondary, capped at `--measure-prose` |
| `.df-boxed-list` | the box: one border, `--radius-lg`, `overflow: hidden` |
| `.df-boxed-list__row` | `min-height: --row-height-comfortable`, hairline `border-top`, first row's suppressed. **Wraps** |
| `.df-boxed-list__text` | label + description column, `flex: 1 1 24ch` |
| `.df-boxed-list__label` / `__description` | the two lines |
| `.df-boxed-list__control` | trailing control, pushed right, wraps below at ≤24ch. `--field` for an input-width control |
| `.df-boxed-list__chevron` | the trailing chevron on an activatable row |
| `.df-boxed-list__slab` | a full-bleed child; strips a nested `.df-list`/`.df-disclosure`'s own border and radius |
| `.df-boxed-list__footer` | a summary strip at the bottom of the box |

- `button.df-boxed-list__row` and `a.df-boxed-list__row` get hover, press and an
  **inset** focus ring (the box clips an outset one, same as `.df-row`).
- Selected is driven by `.is-selected`, `[aria-selected="true"]` or
  `[aria-checked="true"]`, so the visual state cannot drift from the accessible
  one.
- `.df-boxed-list__row` is **not** `.df-row` and has no relationship to
  `--row-height`. Never put a virtualised list's rows in one.
- A `<label>`-wrapped row needs the label text in `.df-boxed-list__label`; when
  the control is a form field, make it a real `<label for>`.

### 6.19 Stepper

The gated step control for the headerbar. Free navigation: a completed step is
reachable in one click.

```html
<nav class="df-stepper" aria-label="Build steps">
  <ol class="df-stepper__list">
    <li class="df-stepper__item">
      <button type="button" class="df-stepper__step is-complete">
        <span class="df-stepper__marker"><svg class="df-stepper__glyph" aria-hidden="true">…tick…</svg></span>
        <span class="df-stepper__label">Target</span>
        <span class="df-visually-hidden">, completed</span>
      </button>
    </li>
    <li class="df-stepper__item">
      <button type="button" class="df-stepper__step" aria-current="step">
        <span class="df-stepper__marker">2</span>
        <span class="df-stepper__label">Packages</span>
      </button>
    </li>
    <li class="df-stepper__item">
      <button type="button" class="df-stepper__step" aria-disabled="true"
              aria-describedby="why-build">
        <span class="df-stepper__marker"><svg class="df-stepper__glyph" aria-hidden="true">…padlock…</svg></span>
        <span class="df-stepper__label">Build</span>
      </button>
      <span class="df-stepper__reason" id="why-build" role="tooltip">
        Choose at least one package first.
      </span>
    </li>
  </ol>
</nav>
```

Three states, each with **at least two signals** so none depends on hue:

| State | Driven by | Signals |
|---|---|---|
| current | `aria-current="step"` (or `.is-current`) | filled accent marker **and** a semibold label **and** a visually-hidden ", current step" |
| completed | `.is-complete` | a tick glyph **and** a tinted marker **and** a visually-hidden ", completed" |
| locked | `aria-disabled="true"` | a padlock glyph **and** a muted label **and** a dashed marker **and** the reason |

**Required of the consumer work:**

- **Never `disabled`.** `aria-disabled="true"` keeps the step focusable, so the
  operator meets it and hears why. The click handler refuses and moves focus to
  the control that will unblock it.
- `.is-complete` is a class and therefore says nothing to assistive technology.
  The visually-hidden ", completed" is not optional.
- **The visually-hidden ", current step" is not optional either**, and this one
  is counter-intuitive: `aria-current="step"` is correct ARIA and it *does*
  reach the platform layer — the accessible carries `current=page` in the
  AT-SPI attribute set. **Orca 46.1 on WebKit2GTK 2.52.6 does not speak it.**
  Measured in the third round against the real binary: standing on the target
  step, Orca announced "1 Target push button" and nothing else, so the one
  thing a gated stepper exists to tell an operator was the one thing only the
  picture carried. Keep the attribute — it is right, and WebView2/NVDA does announce
  it — and add the sentence, because the attribute is not enough on the
  shipping stack. `docs/accessibility.md` §5 has the transcript.
- The reason is a **sibling** of the button, never a child: a child is swallowed
  into the button's accessible name ("Build Choose at least one package first").
  As an `aria-describedby` target it is announced even while visually hidden,
  because the accessible-description computation includes a directly-referenced
  node regardless of visibility.
- It is revealed on **hover *and* focus** — never behind a click that fires a
  toast, which a keyboard operator has to guess at.
- `.df-stepper__reason--end` flips it to the right edge; `.is-open` forces it
  open (the gallery uses this).
- `.df-stepper--compact` moves the labels off screen but **not** out of the
  accessibility tree — it is a clip, not `display: none`, because
  `display: none` would take the button's accessible name with it. Toggle it
  from a `ResizeObserver` at whatever width the labels stop fitting.
- Roving tabindex is *not* applied by the library. Four steps as four tab stops
  is correct; forty chips as forty tab stops is not. Do not add one here.

### 6.20 Toast

**One** pill, from the **bottom centre** of the content area, never stacked.

```html
<div class="df-toast-host">
  <div class="df-toast df-toast--success" role="status" aria-live="polite">
    <svg class="df-toast__icon" aria-hidden="true">…circle-tick…</svg>
    <span class="df-toast__text">Bundle written to /srv/bundles/airgap-2026-09.</span>
    <button type="button" class="df-btn df-btn--secondary df-btn--sm df-toast__action">Open</button>
    <button type="button" class="df-icon-btn df-toast__close" aria-label="Dismiss">…</button>
  </div>
</div>
```

- `.df-toast-host` is `position: absolute`, so it needs a positioned ancestor:
  put it inside `.df-app__main`, which the library makes `position: relative`.
  Anchoring it there rather than to the viewport keeps it off the action bar.
- **`.df-toast-host` holds at most one `.df-toast`.** The queue lives in JS. Two
  toasts in the DOM at once is the bug this component exists to remove — stacked
  bottom-right toasts are a defining web/Electron signature, and GNOME shows one.
- Variants `--info`, `--success`, `--warning`, `--danger` tint the icon only.
  The **shape** is what distinguishes them; supply it.
- `role="alert"` + `aria-live="assertive"` for danger, `role="status"` +
  `aria-live="polite"` for the rest.
- `.is-leaving` plays the exit animation. Never depend on `animationend` to
  remove the node: under reduced motion there is no animation and no event.
- It wraps at a 320px container — the action drops to its own line at ≤16ch.

### 6.21 Popover and contextual help

Layered over content, never inline. This is a **correctness** requirement: an
inline `<details>` above a 60,000-row virtualised list reflows everything below
it, which moves the list's scroll position and keyboard focus every time somebody
touches a filter.

```html
<span class="df-popover-anchor">
  <button type="button" class="df-btn df-btn--secondary"
          aria-expanded="false" aria-controls="pop-sect">Section…</button>
  <div class="df-popover" id="pop-sect" hidden>
    <p class="df-popover__title">Filter by apt Section</p>
    <p class="df-popover__text">…</p>
    <div class="df-popover__footer">…chips…</div>
  </div>
</span>
```

| Class | Contract |
|---|---|
| `.df-popover-anchor` | `position: relative` wrapper. The popover positions against it |
| `.df-popover` | absolute, `--z-dropdown`, surface-3, `--shadow-2`, `--radius-lg` |
| `.df-popover--end` / `--above` | flip to the right edge / open upwards |
| `.df-popover--flush` | no padding, for a menu or list inside |
| `.df-popover--wide` | 40ch minimum instead of 24ch |
| `.df-popover__title` / `__text` / `__footer` | the parts |

The opener owns `aria-expanded` and `aria-controls`. Toggle with `el.hidden`,
never `style.display` (§6.17). The screen closes it on Escape and on an outside
click and puts focus back on the opener.

**Contextual help** is the reason `.df-field__hint` should stop appearing under
every control. A permanent caption under each field is a SaaS-onboarding habit
and half the density problem. Keep the hint only where the answer is short and
always relevant; put the paragraph behind `.df-help__btn` instead.

```html
<span class="df-popover-anchor">
  <button type="button" class="df-help__btn" aria-expanded="false"
          aria-controls="h-name" aria-label="About the bundle name">
    <svg aria-hidden="true">…question mark…</svg>
  </button>
  <div class="df-popover df-popover--end" id="h-name" hidden>
    <p class="df-popover__text">…</p>
  </div>
</span>
```

`.df-help__btn` fills a whole `--hit-target-min` box around a 14px glyph, and it
takes the selected look from `aria-expanded="true"`.

### 6.22 Brand and identity surfaces

```html
<div class="df-headerbar df-brand df-headerbar--brand">
  <svg class="df-brand-mark" viewBox="0 0 32 32" role="img" aria-label="Debark">…</svg>
  <span class="df-wordmark">Debark</span>
  <div class="df-spacer"></div>
  <nav class="df-stepper" aria-label="Build steps">…</nav>
</div>
```

| Class | Contract |
|---|---|
| `.df-brand` | the scope. Re-points surface, row, border and text tokens at their brand equivalents |
| `.df-headerbar--brand` | the headerbar's own hairline, in brand |
| `.df-brand-mark` | sizes the inline logo SVG (`--lg` for 32px) |
| `.df-wordmark` | system font, semibold, `--letter-spacing-brand` (`--lg` for 19px) |
| `.df-about` | the about surface — the one place allowed to look like a poster |
| `.df-about__version` / `__text` / `__rule` | its parts |

**The mark is one SVG, not two.** It is a single 32×32 drawing: a camel whose
outer `fill` and `stroke` are both `currentColor`, carrying a crate whose `rect`
is filled with `var(--color-brand-amber)` through an inline `style` attribute.
Because only the crate is pinned, the mark follows the theme and there is no
light/dark pair to keep in step. `gallery.html` carries the exact markup to
copy, and `shell/shell.js` holds the same paths as `BRAND_MARK`.

**What `.df-brand` deliberately does not re-point:** `--color-accent` and the
four semantic roles. A status means one thing in one hue across the whole app.
`--color-text-disabled` *is* re-pointed, at `--color-brand-text-muted`, because
the cool grey is 2.92:1 on the warm ground; the consequence is that a disabled
control on brand chrome is no dimmer than a secondary one, which is fine because
a control on brand chrome must not be `disabled` at all.

`--color-brand-clay` and `--color-brand-oasis` appear in exactly one place: the
gradient rule on `.df-about`. They must never sit next to a status — §8.2.

### 6.23 Gating — the `aria-disabled` idiom

A gated flow has *more* "you cannot do this yet" states, not fewer, so the
library makes the correct pattern the easy one.

```html
<button type="button" class="df-btn df-btn--primary"
        aria-disabled="true" aria-describedby="build-gate">Build bundle</button>

<!-- announced only -->
<p class="df-live" id="build-gate" role="status" aria-live="polite">
  Choose at least one package first.
</p>

<!-- or on screen as well -->
<p class="df-gate-reason" id="build-gate" role="status" aria-live="polite">
  <svg aria-hidden="true">…padlock…</svg>
  Choose at least one package first.
</p>
```

- `.df-live` — a visually-hidden live region that is still a valid
  `aria-describedby` target.
- `.df-gate-reason` — the same sentence on screen, with an icon slot.
- The library styles `.df-btn[aria-disabled="true"]` exactly like `:disabled`
  **and** suppresses its hover and press states, but never sets
  `pointer-events: none` and never removes it from the tab order. Same for
  `.df-icon-btn` and `.df-chip`.
- The handler refuses the click, **rewrites the live region** so it is
  re-announced (an unchanged live region says nothing), and moves focus to the
  control that will unblock it.
- **The live region and the `aria-describedby` are not belt-and-braces — the
  platform decides which one carries.** Measured in the third round: Orca 46.1
  loads its `WebKitGtk` *toolkit* script for an embedded `WebKitWebView`, and
  live-region support lives only in Orca's *web* script, so on Linux a live
  region is inert — three of them rewritten from the keyboard produced no
  speech at all. What speaks there is the description, read on focus, because a
  directly-referenced node is included in the accessible description whether or
  not it is visible. On Windows it is the other way round: the live region is
  what re-announces a repeated refusal. Dropping either one because "the other
  already says it" makes every gate silent on one of the two shipping platforms.
  `docs/accessibility.md` §5.5 has the transcript.
- **The description is a *pull* channel, not only a one-shot.** Measured in the
  third round: rewriting the `aria-describedby` target while focus stays on the
  control is silent, but the **next** arrival on that control reads the **new** text.
  Driven with Orca on WebKit2GTK: on arrival, "B1 described button push button.
  Phase resolving."; the target was then rewritten in place — silence; Tab away
  and Shift-Tab back — "B1 described button push button. Phase fetching 4 of
  forty." So a region a control describes can carry *changing* state on Linux,
  readable on demand by leaving the control and coming back. It is the closest
  thing to a live region this platform has, and it costs nothing: the markup is
  already the one §2 rule 10 requires. `docs/accessibility.md` §5.7.

### 6.24 The app frame

The window's own layout, published so it stops being forty lines of CSS injected
by the shell at boot.

```html
<body>
  <div id="app" class="df-app-host">            <!-- HEIGHT CONTRACT -->
    <div class="df-app">
      <a class="df-app__skip" href="#main">Skip to content</a>
      <div class="df-headerbar df-brand df-headerbar--brand">…stepper…</div>
      <div class="df-app__banners">…app-level, persistent…</div>
      <main class="df-app__main" id="main">
        <div class="df-app__pane" tabindex="-1">
          <div class="df-app__content">…one step…</div>
        </div>
        <div class="df-toast-host">…at most one .df-toast…</div>
      </main>
      <div class="df-actionbar">…</div>
    </div>
  </div>
</body>
```

| Class | Contract |
|---|---|
| `.df-app-host` | `height: 100%`. **Load-bearing — see below** |
| `.df-app` | the frame. Column flex, full height, `position: relative` |
| `.df-app__skip` | the skip link. First tab stop, visible on focus |
| `.df-app__banners` | app-level persistent banners. `:empty` costs nothing |
| `.df-app__main` | `position: relative`, so `.df-toast-host` anchors to it |
| `.df-app__pane` | one screen. Scrolls; takes programmatic focus on navigation and shows a ring only for keyboard navigation. **It must be a named landmark** — `role="region"` plus `aria-label`, or a `<section>` with `aria-labelledby` — because that is the whole of the step-transition announcement on Linux. See below |
| `.df-app__content` | the readable column: 720px, centred, padded. `--wide` 1080px, `--bleed` for a full-bleed list |

**`.df-app-host` is the fix for a real, measured bug and must not be dropped.**
`.df-app` is `height: 100%`, and a percentage height only resolves against a
parent that has one; `html` and `body` do, the host element a page hands the
shell does not, and CSS cannot select a parent without `:has()`. Leaving it off
does not look broken — it looks like a page that scrolls — but it removes the
bound on every nested scroller, and the virtualised catalogue's port then grows
to `total × --row-height` and renders every row. Measured by the accessibility
pass at 4,000 rows: a 128,000px scroll port with all 4,000 rows in the DOM. At the real
catalogue size that is 1.92 million pixels and 60,000 rows, and it takes the
frame-rate and the 400MB memory budgets with it.

**`.df-app__pane` being a named landmark is not tidiness — it is the
announcement.** ARIA live regions are inert under Orca on WebKit2GTK
(`docs/accessibility.md` §5.5), so the shell's `say()` on navigation is silent
and the only thing an operator hears when a step changes is Orca reading the
landmark that focus landed in: `SAID: 'landmark Choose a target.'`. The shell
gives every pane `tabindex="-1"`, `role="region"` and an `aria-label`, and all
three are load-bearing. Drop the name and the pane stops being a landmark, and
a step transition announces nothing at all — which is a silent regression, not
a visible one. §2 rule 13 has the four shapes that were driven to establish
this, including the one that looks obviously right and is mute.

`.df-app__content` is the answer to "a step is a form, not a dashboard": give it
a measure and centre it rather than letting four controls stretch across 1600px.
A pane whose job is the full-bleed catalogue uses `--bleed` and opts out.

### Why not `html:has(.df-app)` instead of `.df-app-host`?

Raised because the sentence above says "CSS cannot select a parent without
`:has()`", and `:has()` **is** available on the recorded package-baseline engine,
WebKitGTK 2.44, and the older 2.36 compatibility engine. The feature table was
verified through GObject bindings when the `light-dark()` question was
settled (§3). So the rule could in principle be answered in the stylesheet:
`html:has(.df-app), html:has(.df-app) body, html:has(.df-app) body > *
{ height: 100% }`, and `index.html` would need no class at all.

**It should not be done, and the reason is not `:has()`.** Three arguments, in
increasing order of weight:

1. **`:has()` is excluded on style grounds anyway** (§1), alongside `@layer`.
   That exclusion survived the third round deliberately: two of the four original
   exclusions turned out to be unnecessary on compatibility grounds and were
   kept for readability. Reaching for the one selector this file has decided
   not to use, to save one class attribute, inverts that decision for no gain.
2. **It would move a contract from a place a reader looks to a place they do
   not.** `class="df-app-host"` on `#app` is visible in `index.html`, which is
   fifteen lines long, and it names the requirement. A `:has()` rule buried in a
   2,600-line stylesheet is invisible to whoever writes the next host page —
   and a host page is exactly what a demo harness, a screenshot rig or an
   embedding is.
3. **The one that settles it: this is a load-bearing fix for a shipped bug, and
   the failure mode is silent.** The height collapse did not look broken; it
   looked like a page that scrolls, while the virtualiser's port grew to the
   whole result set — 4,000 of 4,000 rows in the DOM, measured, and 60,000 at
   the real catalogue size. A change that swaps a mechanism for a newer one is
   a behaviour change even when it is meant to be equivalent, and the cost of
   being wrong here is that the bug comes back invisibly, months later, in
   whichever host page the selector happens not to match. `index.html` is also
   not this package file.

The honest summary: `:has()` **would** work on every engine this package can
install on, and that is worth writing down so nobody has to re-derive it. It is
still the wrong trade. If a future pass does it anyway, the acceptance test is
not "the app looks right" — it is the row count: mount into a host with no
resolved height and prove the DOM holds tens of rows, not thousands.

---

## 7. Measured contrast

Every number below is **computed from the hex values in `tokens.css`** by the
audit script described in §7.3, using the WCAG 2.x relative-luminance formula.
Nothing here is carried over from a previous version of this file: the whole set
was recomputed when the brand tokens landed, because a new surface changes every
pair measured against it.

**146 pairs, each measured in both themes — 292 measurements. All pass.**

Six of the 146 carry no floor and are reported for information only; see the
`informational` group below. §7.4 adds a further 26 pairs — 52 measurements —
that only carry a floor under `prefers-contrast: more`; three of those pairs
are new here and the rest are re-reads of rows in §7.2.

| Set | Pairs | Floor | Worst light | Worst dark |
|---|---|---|---|---|
| Body text on surfaces and tints | 20 | 4.5:1 | **5.57:1** (`--color-text-secondary` on `--color-row-active`) | **5.67:1** (`--color-text-secondary` on `--color-row-active`) |
| Role text | 20 | 4.5:1 | **5.38:1** (`--color-accent-text` on `--color-accent-bg`) | **5.93:1** (`--color-danger-text` on `--color-surface-3`) |
| Text on solid fills | 9 | 4.5:1 | **4.65:1** (`--color-info-on-fill` on `--color-info-fill`) | **4.56:1** (`--color-danger-on-fill` on `--color-danger-fill-hover`) |
| Borders and the focus ring | 25 | 3.0:1 | **3.02:1** (`--color-border-default` on `--color-row-active`) | **3.02:1** (`--color-border-default` on `--color-row-active`) |
| Informational — the focus halo against the fill it sits on | 6 | none | **6.18:1** (`--color-focus-halo` on `--color-danger-fill`) | **2.71:1** (`--color-focus-halo` on `--color-danger-fill-active`) |
| Role icon / rule colours | 20 | 3.0:1 | **3.05:1** (`--color-warning` on `--color-warning-bg`) | **3.63:1** (`--color-danger` on `--color-surface-3`) |
| Text on brand chrome (new) | 20 | 4.5:1 | **4.76:1** (`--color-brand-text-muted` on `--color-brand-active`) | **4.90:1** (`--color-brand-text-muted` on `--color-brand-active`) |
| Borders and role colours on brand chrome (new) | 26 | 3.0:1 | **3.06:1** (`--color-warning` on `--color-brand-surface`) | **3.60:1** (`--color-brand-border-control` on `--color-brand-active`) |

### 7.1 Deliberate exclusions

Five tokens are excluded, and each is excluded for a stated reason rather than
because it failed.

| Token | light | dark | Why it is exempt |
|---|---|---|---|
| `--color-border-subtle` | 1.20:1 | 1.36:1 | decorative row/section separator (WCAG 1.4.11 exempt) |
| `--color-brand-border` | 1.39:1 | 1.26:1 | decorative chrome hairline (WCAG 1.4.11 exempt) |
| `--color-brand-amber` | 2.01:1 | 7.78:1 | decorative fill / logo crate only, never text or a state |
| `--color-brand-clay` | 2.38:1 | 6.59:1 | decorative fill only, about surface |
| `--color-brand-oasis` | 2.01:1 | 7.79:1 | decorative fill only, about surface |

Ratios in that table are against `--color-surface-1`. WCAG 1.4.11 requires 3:1
only for a boundary needed to *identify* a control; a row separator and a chrome
hairline are decorative and are exempt. The three fixed brand hues are exempt
because they may only ever be a decorative fill — never text, never a border a
reader has to find, never a state. **If a screen presses any of them into service
as a control boundary or a status, that is a real failure**, and it is the one
thing about this palette worth a lint rule.

### 7.2 Every pair

#### Body text on surfaces and tints — threshold 4.5:1 (20 pairs)

| foreground | background | light | dark |
|---|---|---|---|
| `--color-text-primary` | `--color-surface-0` | **14.53:1** | **15.68:1** |
| `--color-text-primary` | `--color-surface-1` | **16.17:1** | **14.44:1** |
| `--color-text-primary` | `--color-surface-2` | **17.82:1** | **13.04:1** |
| `--color-text-primary` | `--color-surface-3` | **17.82:1** | **11.59:1** |
| `--color-text-primary` | `--color-surface-inset` | **15.47:1** | **16.16:1** |
| `--color-text-primary` | `--color-row-hover` | **15.86:1** | **11.75:1** |
| `--color-text-primary` | `--color-row-active` | **14.48:1** | **10.50:1** |
| `--color-text-primary` | `--color-row-selected` | **15.07:1** | **12.36:1** |
| `--color-text-secondary` | `--color-surface-1` | **6.21:1** | **7.80:1** |
| `--color-text-secondary` | `--color-surface-2` | **6.85:1** | **7.04:1** |
| `--color-text-secondary` | `--color-surface-3` | **6.85:1** | **6.26:1** |
| `--color-text-secondary` | `--color-surface-inset` | **5.95:1** | **8.73:1** |
| `--color-text-secondary` | `--color-row-hover` | **6.10:1** | **6.35:1** |
| `--color-text-secondary` | `--color-row-active` | **5.57:1** | **5.67:1** |
| `--color-text-secondary` | `--color-row-selected` | **5.79:1** | **6.67:1** |
| `--color-text-primary` | `--color-accent-bg` | **15.07:1** | **12.65:1** |
| `--color-text-primary` | `--color-info-bg` | **15.21:1** | **12.51:1** |
| `--color-text-primary` | `--color-success-bg` | **15.27:1** | **12.40:1** |
| `--color-text-primary` | `--color-warning-bg` | **15.65:1** | **12.65:1** |
| `--color-text-primary` | `--color-danger-bg` | **14.61:1** | **13.77:1** |

#### Role text — threshold 4.5:1 (20 pairs)

| foreground | background | light | dark |
|---|---|---|---|
| `--color-accent-text` | `--color-surface-1` | **5.77:1** | **7.85:1** |
| `--color-accent-text` | `--color-surface-2` | **6.36:1** | **7.09:1** |
| `--color-accent-text` | `--color-surface-3` | **6.36:1** | **6.30:1** |
| `--color-accent-text` | `--color-accent-bg` | **5.38:1** | **6.88:1** |
| `--color-info-text` | `--color-surface-1` | **6.12:1** | **9.42:1** |
| `--color-info-text` | `--color-surface-2` | **6.75:1** | **8.51:1** |
| `--color-info-text` | `--color-surface-3` | **6.75:1** | **7.56:1** |
| `--color-info-text` | `--color-info-bg` | **5.76:1** | **8.16:1** |
| `--color-success-text` | `--color-surface-1` | **6.40:1** | **10.03:1** |
| `--color-success-text` | `--color-surface-2` | **7.05:1** | **9.05:1** |
| `--color-success-text` | `--color-surface-3` | **7.05:1** | **8.05:1** |
| `--color-success-text` | `--color-success-bg` | **6.04:1** | **8.61:1** |
| `--color-warning-text` | `--color-surface-1` | **5.87:1** | **12.64:1** |
| `--color-warning-text` | `--color-surface-2` | **6.47:1** | **11.41:1** |
| `--color-warning-text` | `--color-surface-3` | **6.47:1** | **10.15:1** |
| `--color-warning-text` | `--color-warning-bg` | **5.68:1** | **11.07:1** |
| `--color-danger-text` | `--color-surface-1` | **7.61:1** | **7.39:1** |
| `--color-danger-text` | `--color-surface-2` | **8.39:1** | **6.67:1** |
| `--color-danger-text` | `--color-surface-3` | **8.39:1** | **5.93:1** |
| `--color-danger-text` | `--color-danger-bg` | **6.88:1** | **7.05:1** |

#### Text on solid fills — threshold 4.5:1 (9 pairs)

| foreground | background | light | dark |
|---|---|---|---|
| `--color-accent-on-fill` | `--color-accent-fill` | **6.36:1** | **4.97:1** |
| `--color-info-on-fill` | `--color-info-fill` | **4.65:1** | **4.95:1** |
| `--color-success-on-fill` | `--color-success-fill` | **5.69:1** | **4.99:1** |
| `--color-warning-on-fill` | `--color-warning-fill` | **5.16:1** | **12.80:1** |
| `--color-danger-on-fill` | `--color-danger-fill` | **6.18:1** | **5.02:1** |
| `--color-accent-on-fill` | `--color-accent-fill-hover` | **7.47:1** | **4.60:1** |
| `--color-accent-on-fill` | `--color-accent-fill-active` | **8.74:1** | **5.75:1** |
| `--color-danger-on-fill` | `--color-danger-fill-hover` | **7.34:1** | **4.56:1** |
| `--color-danger-on-fill` | `--color-danger-fill-active` | **8.51:1** | **5.76:1** |

#### Borders and the focus ring — threshold 3.0:1 (25 pairs)

| foreground | background | light | dark |
|---|---|---|---|
| `--color-border-default` | `--color-surface-0` | **3.03:1** | **4.51:1** |
| `--color-border-default` | `--color-surface-1` | **3.38:1** | **4.15:1** |
| `--color-border-default` | `--color-surface-2` | **3.72:1** | **3.75:1** |
| `--color-border-default` | `--color-surface-3` | **3.72:1** | **3.33:1** |
| `--color-border-default` | `--color-row-hover` | **3.31:1** | **3.38:1** |
| `--color-border-default` | `--color-row-active` | **3.02:1** | **3.02:1** |
| `--color-border-default` | `--color-row-selected` | **3.15:1** | **3.55:1** |
| `--color-border-strong` | `--color-surface-1` | **5.78:1** | **5.72:1** |
| `--color-border-strong` | `--color-surface-2` | **6.37:1** | **5.16:1** |
| `--color-border-strong` | `--color-surface-3` | **6.37:1** | **4.59:1** |
| `--color-border-focus` | `--color-surface-0` | **5.18:1** | **8.53:1** |
| `--color-border-focus` | `--color-surface-1` | **5.77:1** | **7.85:1** |
| `--color-border-focus` | `--color-surface-2` | **6.36:1** | **7.09:1** |
| `--color-border-focus` | `--color-surface-3` | **6.36:1** | **6.30:1** |
| `--color-border-focus` | `--color-surface-inset` | **5.52:1** | **8.79:1** |
| `--color-border-focus` | `--color-row-hover` | **5.66:1** | **6.39:1** |
| `--color-border-focus` | `--color-row-active` | **5.17:1** | **5.71:1** |
| `--color-border-focus` | `--color-row-selected` | **5.38:1** | **6.72:1** |
| `--color-border-focus` | `--color-accent-bg` | **5.38:1** | **6.88:1** |
| `--color-focus-ring` | `--color-surface-1` | **5.77:1** | **7.85:1** |
| `--color-focus-ring` | `--color-surface-2` | **6.36:1** | **7.09:1** |
| `--color-focus-ring` | `--color-surface-3` | **6.36:1** | **6.30:1** |
| `--color-focus-ring` | `--color-focus-halo` | **6.36:1** | **7.09:1** |
| `--color-text-disabled` | `--color-surface-1` | **3.11:1** | **3.98:1** |
| `--color-text-disabled` | `--color-surface-2` | **3.43:1** | **3.59:1** |

#### Informational — the focus halo against the fill it sits on — no floor (6 pairs)

The halo is a **spacer**, not the indicator. What has to be identifiable is the
ring, and the ring's own pairs are `focus-ring` against `focus-halo` (6.36:1 light,
7.09:1 dark) and `border-focus` against every surface — all in the group above. The
halo against the fill underneath it is reported because it is visible, not
because anything depends on it.

| foreground | background | light | dark |
|---|---|---|---|
| `--color-focus-halo` | `--color-accent-fill` | **6.36:1** | **3.14:1** |
| `--color-focus-halo` | `--color-accent-fill-hover` | **7.47:1** | **3.39:1** |
| `--color-focus-halo` | `--color-accent-fill-active` | **8.74:1** | **2.71:1** |
| `--color-focus-halo` | `--color-danger-fill` | **6.18:1** | **3.11:1** |
| `--color-focus-halo` | `--color-danger-fill-hover` | **7.34:1** | **3.42:1** |
| `--color-focus-halo` | `--color-danger-fill-active` | **8.51:1** | **2.71:1** |

#### Role icon / rule colours — threshold 3.0:1 (20 pairs)

| foreground | background | light | dark |
|---|---|---|---|
| `--color-accent` | `--color-surface-1` | **5.77:1** | **7.85:1** |
| `--color-accent` | `--color-surface-2` | **6.36:1** | **7.09:1** |
| `--color-accent` | `--color-surface-3` | **6.36:1** | **6.30:1** |
| `--color-accent` | `--color-accent-bg` | **5.38:1** | **6.88:1** |
| `--color-info` | `--color-surface-1` | **4.21:1** | **9.42:1** |
| `--color-info` | `--color-surface-2` | **4.65:1** | **8.51:1** |
| `--color-info` | `--color-surface-3` | **4.65:1** | **7.56:1** |
| `--color-info` | `--color-info-bg` | **3.96:1** | **8.16:1** |
| `--color-success` | `--color-surface-1` | **4.21:1** | **9.48:1** |
| `--color-success` | `--color-surface-2` | **4.64:1** | **8.56:1** |
| `--color-success` | `--color-surface-3` | **4.64:1** | **7.61:1** |
| `--color-success` | `--color-success-bg` | **3.97:1** | **8.14:1** |
| `--color-warning` | `--color-surface-1` | **3.15:1** | **12.35:1** |
| `--color-warning` | `--color-surface-2` | **3.47:1** | **11.15:1** |
| `--color-warning` | `--color-surface-3` | **3.47:1** | **9.91:1** |
| `--color-warning` | `--color-warning-bg` | **3.05:1** | **10.82:1** |
| `--color-danger` | `--color-surface-1` | **8.36:1** | **4.53:1** |
| `--color-danger` | `--color-surface-2` | **9.22:1** | **4.09:1** |
| `--color-danger` | `--color-surface-3` | **9.22:1** | **3.63:1** |
| `--color-danger` | `--color-danger-bg` | **7.56:1** | **4.32:1** |

#### Text on brand chrome (new) — threshold 4.5:1 (20 pairs)

| foreground | background | light | dark |
|---|---|---|---|
| `--color-brand-text` | `--color-brand-surface` | **12.56:1** | **13.13:1** |
| `--color-brand-text-muted` | `--color-brand-surface` | **5.91:1** | **6.02:1** |
| `--color-text-primary` | `--color-brand-surface` | **15.73:1** | **14.89:1** |
| `--color-text-secondary` | `--color-brand-surface` | **6.05:1** | **8.04:1** |
| `--color-brand-text` | `--color-brand-canvas` | **13.33:1** | **13.96:1** |
| `--color-brand-text-muted` | `--color-brand-canvas` | **6.27:1** | **6.40:1** |
| `--color-text-primary` | `--color-brand-canvas` | **16.69:1** | **15.83:1** |
| `--color-text-secondary` | `--color-brand-canvas` | **6.42:1** | **8.55:1** |
| `--color-brand-text` | `--color-brand-hover` | **11.11:1** | **11.88:1** |
| `--color-brand-text-muted` | `--color-brand-hover` | **5.22:1** | **5.45:1** |
| `--color-text-primary` | `--color-brand-hover` | **13.91:1** | **13.48:1** |
| `--color-text-secondary` | `--color-brand-hover` | **5.35:1** | **7.28:1** |
| `--color-brand-text` | `--color-brand-active` | **10.12:1** | **10.68:1** |
| `--color-brand-text-muted` | `--color-brand-active` | **4.76:1** | **4.90:1** |
| `--color-text-primary` | `--color-brand-active` | **12.68:1** | **12.11:1** |
| `--color-text-secondary` | `--color-brand-active` | **4.87:1** | **6.54:1** |
| `--color-brand-accent` | `--color-brand-surface` | **4.83:1** | **8.26:1** |
| `--color-accent-text` | `--color-brand-surface` | **5.61:1** | **8.10:1** |
| `--color-brand-accent` | `--color-brand-canvas` | **5.12:1** | **8.78:1** |
| `--color-accent-text` | `--color-brand-canvas` | **5.95:1** | **8.61:1** |

#### Borders and role colours on brand chrome (new) — threshold 3.0:1 (26 pairs)

| foreground | background | light | dark |
|---|---|---|---|
| `--color-brand-border-control` | `--color-brand-surface` | **3.92:1** | **4.43:1** |
| `--color-border-strong` | `--color-brand-surface` | **5.62:1** | **5.90:1** |
| `--color-focus-ring` | `--color-brand-surface` | **5.61:1** | **8.10:1** |
| `--color-border-focus` | `--color-brand-surface` | **5.61:1** | **8.10:1** |
| `--color-brand-border-control` | `--color-brand-canvas` | **4.16:1** | **4.71:1** |
| `--color-border-strong` | `--color-brand-canvas` | **5.97:1** | **6.27:1** |
| `--color-focus-ring` | `--color-brand-canvas` | **5.95:1** | **8.61:1** |
| `--color-border-focus` | `--color-brand-canvas` | **5.95:1** | **8.61:1** |
| `--color-brand-border-control` | `--color-brand-hover` | **3.47:1** | **4.01:1** |
| `--color-border-strong` | `--color-brand-hover` | **4.97:1** | **5.34:1** |
| `--color-focus-ring` | `--color-brand-hover` | **4.96:1** | **7.33:1** |
| `--color-border-focus` | `--color-brand-hover` | **4.96:1** | **7.33:1** |
| `--color-brand-border-control` | `--color-brand-active` | **3.16:1** | **3.60:1** |
| `--color-border-strong` | `--color-brand-active` | **4.53:1** | **4.80:1** |
| `--color-focus-ring` | `--color-brand-active` | **4.52:1** | **6.59:1** |
| `--color-border-focus` | `--color-brand-active` | **4.52:1** | **6.59:1** |
| `--color-accent` | `--color-brand-surface` | **5.61:1** | **8.10:1** |
| `--color-danger` | `--color-brand-surface` | **8.14:1** | **4.67:1** |
| `--color-warning` | `--color-brand-surface` | **3.06:1** | **12.73:1** |
| `--color-success` | `--color-brand-surface` | **4.09:1** | **9.78:1** |
| `--color-info` | `--color-brand-surface` | **4.10:1** | **9.72:1** |
| `--color-accent` | `--color-brand-canvas` | **5.95:1** | **8.61:1** |
| `--color-danger` | `--color-brand-canvas` | **8.63:1** | **4.96:1** |
| `--color-warning` | `--color-brand-canvas` | **3.25:1** | **13.54:1** |
| `--color-success` | `--color-brand-canvas` | **4.34:1** | **10.40:1** |
| `--color-info` | `--color-brand-canvas` | **4.35:1** | **10.33:1** |

### 7.3 Reproducing this

The audit is a dependency-free Python script (`colour.py` + `audit.py`) that
parses `tokens.css` directly, so the numbers cannot drift from the CSS without
the script saying so. It reads each colour as `light-dark(<light>, <dark>)` and
takes the first arm for the light column and the second for the dark one.

The earlier palette collapse removed its duplicate-dark-block assertion.
The compatibility fallback in §3 makes parity checks necessary again: compare
all 53 light/dark fallback values with the modern arms, compare the two dark
fallback blocks, and verify that later contrast rules retain precedence. The
desktop UX change ran those checks against the committed palette; future
palette edits must repeat them alongside rendered-engine checks.

The historical audit script was not committed. Reproducing its calculations
requires the WCAG formula, the Viénot matrices in §8 and CIEDE2000. The current
committed review harness and its rendered-state limits are documented in
[ui-review.md](../../../docs/dev/ui-review.md).

The historical palette audit skipped media queries and attribute selectors;
current audits must also distinguish the `@supports-not` fallback block.
Adding the `prefers-contrast: more` block did not disturb the historical audit: it
re-points tokens at other tokens and declares no colour of its own, so the
palette the script reads is still the bare `:root` one. §7.4's numbers come
from the same parser, and it was checked against three rows already published
in §7.2 before any of them were believed —
`--color-text-primary`/`--color-surface-1` 16.17 / 14.44,
`--color-border-default`/`--color-row-active` 3.02 / 3.02,
`--color-focus-ring`/`--color-brand-active` 4.52 / 6.59 — all exact.

**If you change any surface or text colour, every pair against it must be
re-measured.** That is not a formality — `--color-brand-surface` was chosen at
the lightest value where `--color-warning` still clears 3:1 on it (3.06:1); a
warmer, deeper sand looked better and put a status icon in the headerbar under
the floor.

### 7.4 Under `prefers-contrast: more`

`tokens.css` answers `prefers-contrast: more` — the query Linux actually
signals, since `forced-colors` does not exist on WebKitGTK — by promoting the
only two colours in the palette that sit below 3:1. Both are the decorative
hairlines §7.1 exempts, and under `more` they stop being decorative, because
the operator has asked to be able to see edges.

```css
@media (prefers-contrast: more) {
  :root {
    --color-border-subtle: var(--color-border-default);
    --color-brand-border: var(--color-brand-border-control);
    --focus-ring-width: 3px;
  }
}
```

**No new colour is introduced.** Each promoted token is re-pointed at a colour
this file already defines and this section already measures, so the CVD solve in
§8 is untouched and the 292 measurements above still describe the palette. The
focus ring changes width only, and a width is not a pair.

| foreground | background | light | dark |
|---|---|---|---|
| `--color-border-default` | `--color-surface-0` | **3.03:1** | **4.51:1** |
| `--color-border-default` | `--color-surface-1` | **3.38:1** | **4.15:1** |
| `--color-border-default` | `--color-surface-2` | **3.72:1** | **3.75:1** |
| `--color-border-default` | `--color-surface-3` | **3.72:1** | **3.33:1** |
| `--color-border-default` | `--color-surface-inset` | **3.23:1** | **4.65:1** | *new* |
| `--color-border-default` | `--color-row-hover` | **3.31:1** | **3.38:1** |
| `--color-border-default` | `--color-row-active` | **3.02:1** | **3.02:1** |
| `--color-border-default` | `--color-row-selected` | **3.15:1** | **3.55:1** |
| `--color-brand-border-control` | `--color-brand-surface` | **3.92:1** | **4.43:1** |
| `--color-brand-border-control` | `--color-brand-canvas` | **4.16:1** | **4.71:1** |
| `--color-brand-border-control` | `--color-brand-hover` | **3.47:1** | **4.01:1** |
| `--color-brand-border-control` | `--color-brand-active` | **3.16:1** | **3.60:1** |
| `--color-brand-border-control` | `--color-surface-0` | **3.62:1** | **4.66:1** | *new* |
| `--color-brand-border-control` | `--color-surface-1` | **4.03:1** | **4.29:1** | *new* |

The last two are new because the headerbar's hairline has two sides: it is
`--color-brand-border` drawn at the boundary between brand chrome and the
content surface below it, and §7.2 only ever measured it against brand chrome.
`--color-border-default` on `--color-surface-inset` is new for the same kind of
reason — a `.df-log` or a `.df-code` sits on the inset surface and now has a
border with a floor.

The twelve focus-ring pairs are unchanged in colour and are re-read here only
because the ring is drawn thicker: `--color-focus-ring` against the eight
surfaces and the four brand tints, worst **4.52:1** light
(`--color-brand-active`), worst **5.71:1** dark (`--color-row-active`) — the
same numbers as §7.2 and `docs/accessibility.md` §9.

```
pairs under prefers-contrast: more: 52   failures: 0   worst case: 3.02:1
the same tokens without it:         28   below 3:1: 14  (the §7.1 exclusions)
```

**What the block deliberately does not do.** It does not repaint text: the worst
text pair in the palette is 5.57:1 against a 4.5:1 floor, and promoting
`--color-text-secondary` would flatten the label/description hierarchy every
`.df-boxed-list__row` is built from in order to fix nothing. It does not thicken
`--border-width-hairline`: a border is in the box model, so doubling 31 of them
is a layout change that would need the whole five-screen × eight-case reflow
matrix re-run. The outline is not in the box model, which is why the ring can
thicken for free.

**One consequence, stated rather than hidden.** `--color-border-subtle` is also
the border of a `:disabled` or `aria-disabled` control, so under `more` a gated
control's boundary becomes as visible as an enabled one's. That is right here:
this system never carried the state in that border — §6.23 bans `disabled`
outright and puts the state in `aria-disabled`, a dimmed label, a padlock glyph
and a spoken reason — and WCAG 1.4.11 exempts a disabled control from the floor
in either direction.

**A cascade trap this block had to avoid, measured rather than assumed.** A
`var()` inside a custom property resolves against the **cascaded** value of what
it references, so `--a: var(--b); --b: var(--c)` in one rule collapses *both* to
`--c`. Driven on WebKit2GTK 2.52.6 with a three-token probe: rule off →
`#111 / #222 / #333`; rule on → `#333 / #333 / #333`. Neither target above is a
token this rule re-points, which is why `--color-border-subtle` promotes to
`--color-border-default` and stops there rather than promoting
`--color-border-default` to `--color-border-strong` in the same breath.

Historically verified painted, not just declared, on WebKitGTK **2.52.6**
(Ubuntu 24.04 review environment) and **2.44.0** (the recorded Ubuntu 24.04 GA
package baseline), by writing real GTK settings
and reading `getComputedStyle` back off `.df-boxed-list`, `.df-list`,
`.df-headerbar--brand`, `.df-toolbar__divider` and a forced-focus `.df-btn`:

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

and then in the **shipping binary** itself, rebuilt from this tree with
`hack/copyfrontend` and `-tags "desktop,production,webkit2_41"`: the readiness
screen photographed at 1400×900 with and without `gtk-theme-name=HighContrast`
differs in **11,910 pixels** at identical geometry, which is the separators and
the ring and nothing else.

---

## 8. Colour-vision deficiency

### 8.1 The alert colours

The alert colours were **solved numerically against a dichromacy simulation**,
not picked by eye: each was binary-searched so that its *simulated* relative
luminance lands on a deliberately spaced target, because a WCAG contrast ratio
is a luminance measure and simulated deuteranopia pulls dark reds and mid greens
toward the same luminance.

That solve was **re-run for this palette**, not quoted from the previous pass.

**Method.** Viénot, Brettel & Mollon (1999) single-plane LMS projection, applied
in **linear light** (sRGB is linearised, projected, and re-encoded). Two numbers
per pair: the WCAG ratio between the two *simulated* colours — a pure lightness
separation, which survives the loss of hue — and CIEDE2000 ΔE, a perceptual
colour difference. Either being large is enough to tell the pair apart.

> **An honest note on the numbers.** These differ slightly from the figures this
> file published before the brand pass — deuteranopia warning/danger reads
> 2.46:1 here against 2.37:1 there, and the tritanopia column differs more. The
> palette did not change; the simulation did. The 1999 paper publishes plane
> matrices for protanopia and deuteranopia only, the tritan plane comes from
> Brettel's two-plane model, and implementations differ on whether the
> projection runs in linear or gamma-encoded light (this one uses linear, which
> is the physically correct reading). Both sets of numbers support the same
> conclusion; only this one is reproducible from the script in §7.3.

| pair | protanopia | deuteranopia | tritanopia |
|---|---|---|---|
| **light theme** | | | |
| warning vs danger | 3.20:1 / ΔE 31 | 2.46:1 / ΔE 25 | 2.13:1 / ΔE 21 |
| success vs danger | 3.01:1 / ΔE 26 | 1.62:1 / ΔE 15 | 1.21:1 / ΔE 65 |
| success vs warning | 1.06:1 / ΔE 10 | 1.51:1 / ΔE 19 | 1.75:1 / ΔE 73 |
| info vs success | 1.02:1 / ΔE 48 | 1.02:1 / ΔE 45 | 1.02:1 / ΔE 4 |
| info vs danger | 2.95:1 / ΔE 49 | 1.66:1 / ΔE 57 | 1.19:1 / ΔE 69 |
| accent vs info | 1.40:1 / ΔE 10 | 1.36:1 / ΔE 8 | 1.32:1 / ΔE 7 |
| **dark theme** | | | |
| warning vs danger | 3.80:1 / ΔE 34 | 2.38:1 / ΔE 22 | 1.72:1 / ΔE 14 |
| success vs danger | 3.44:1 / ΔE 32 | 1.68:1 / ΔE 19 | 1.00:1 / ΔE 64 |
| success vs warning | 1.11:1 / ΔE 5 | 1.42:1 / ΔE 14 | 1.73:1 / ΔE 68 |
| info vs success | 1.06:1 / ΔE 42 | 1.02:1 / ΔE 39 | 1.04:1 / ΔE 1 |
| info vs danger | 3.24:1 / ΔE 52 | 1.72:1 / ΔE 56 | 1.04:1 / ΔE 63 |
| accent vs info | 1.21:1 / ΔE 6 | 1.19:1 / ΔE 6 | 1.15:1 / ΔE 4 |

**warning vs danger** — the pair the brief calls out, and the one most likely to
matter in this app — holds 2.46:1 / ΔE 25 (light) and 2.38:1 / ΔE 22 (dark) under deuteranopia. An
earlier hand-picked palette put this pair at **1.00:1 / ΔE 0**: literally the
same colour. That is what the numeric solve fixed, and re-running it here
confirms the fix survived the brand work.

**The honest caveat, unchanged.** Under **tritanopia** — rare, well under 0.1%
of people — `info` and `success` converge: 1.02:1 / ΔE 4 light, 1.04:1 / ΔE 1 dark. Blue and green
share a fate when the S-cone is missing, and separating them would mean giving
up either the blue accent or the green success, both strong conventions for this
audience. This is accepted, and it is exactly why rule 2 exists: **every status
in this app carries an icon shape and a text label, so no state depends on hue.**

`accent` vs `info` is also close in every simulation (1.36:1 / ΔE 8 under deuteranopia in
light). They are both blue and are meant to be; `accent` means "you can act on
this" and `info` means "here is a fact", and the two never appear as alternatives
to each other in the same control.

### 8.2 Brand hue vs semantic hue — why the quarantine exists

This is the measurement that decided the palette. The brief warned that brand
clay sits in the same warm family as `warning`/`danger` and brand oasis near
`success`. It does, and worse than "similar":

| pair | normal vision | protanopia | deuteranopia | tritanopia |
|---|---|---|---|---|
| **light theme** | | | | |
| brand-amber vs warning | 1.56:1 / ΔE 12 | 1.61:1 / ΔE 12 | 1.55:1 / ΔE 11 | 1.46:1 / ΔE 10 |
| brand-amber vs danger | 4.16:1 / ΔE 50 | 5.15:1 / ΔE 46 | 3.81:1 / ΔE 38 | 3.10:1 / ΔE 31 |
| brand-clay vs warning | 1.32:1 / ΔE 19 | 1.30:1 / ΔE 14 | 1.33:1 / ΔE 12 | 1.33:1 / ΔE 7 |
| brand-clay vs danger | 3.52:1 / ΔE 36 | 4.16:1 / ΔE 36 | 3.28:1 / ΔE 34 | 2.82:1 / ΔE 28 |
| brand-oasis vs success | 2.09:1 / ΔE 20 | 2.08:1 / ΔE 19 | 2.11:1 / ΔE 20 | 1.88:1 / ΔE 17 |
| brand-oasis vs info | 2.10:1 / ΔE 45 | 2.12:1 / ΔE 50 | 2.07:1 / ΔE 46 | 1.92:1 / ΔE 19 |
| **dark theme** | | | | |
| brand-amber vs warning | 1.59:1 / ΔE 13 | 1.67:1 / ΔE 13 | 1.54:1 / ΔE 12 | 1.43:1 / ΔE 9 |
| brand-amber vs danger | 1.72:1 / ΔE 33 | 2.28:1 / ΔE 24 | 1.55:1 / ΔE 11 | 1.20:1 / ΔE 5 |
| brand-clay vs warning | 1.87:1 / ΔE 25 | 2.07:1 / ΔE 19 | 1.79:1 / ΔE 15 | 1.58:1 / ΔE 11 |
| brand-clay vs danger | 1.46:1 / ΔE 15 | 1.84:1 / ΔE 17 | 1.33:1 / ΔE 10 | 1.09:1 / ΔE 2 |
| brand-oasis vs success | 1.22:1 / ΔE 6 | 1.24:1 / ΔE 6 | 1.21:1 / ΔE 5 | 1.13:1 / ΔE 4 |
| brand-oasis vs info | 1.21:1 / ΔE 36 | 1.17:1 / ΔE 40 | 1.23:1 / ΔE 38 | 1.17:1 / ΔE 5 |

Read the ΔE column. **Brand amber against `warning` is ΔE 12 in light and ΔE 13
in dark under normal vision** — for reference, ΔE 2.3 is the classic
just-noticeable difference and ΔE 12 is "the same colour, slightly off". In dark
theme **brand oasis against `success` is ΔE 6**, and **brand clay against
`danger` is ΔE 2 under tritanopia**. Two of those are indistinguishable to
everyone, not only to a dichromat.

So the resolution is the second of the two the brief allowed: **brand hues are
kept off working surfaces entirely.** They live inside `.df-brand`, `.df-about`
and the logo mark, and the CSS never puts one next to a status. There is no
version of this palette where an amber fill and a `warning` icon can share a
screen region and still mean two different things.

---

## 9. Accessibility summary

- **Focus.** Every interactive element has a visible ring. `outline: none`
  appears twice in the library, never without a replacement: on
  `:focus:not(:focus-visible)` (which drops the fallback ring for pointer focus
  on engines that support `:focus-visible`, the next rule re-applying it for
  keyboard), and on `.df-search__input` (the ring is drawn on the wrapper). A
  full-width list row draws its ring **inset**, because the list clips an outset
  one.
- **`:focus-visible` fallback.** The library sets a ring on `:focus` first, then
  removes it for pointer focus via `:focus:not(:focus-visible)`. On an engine
  without `:focus-visible` support this degrades to a ring on all focus, which
  is the safe direction.
- **Contrast.** 146 pairs measured in both themes — 292 measurements — all
  passing, and all recomputed from `tokens.css` rather than carried over. §7.
- **Colour is never the only signal** — §8, and rule 2.
- **Reduced motion.** Every duration is a token and every token collapses;
  looping animations stop outright.
- **Forced colours.** Both files hand the palette back to the OS under
  `forced-colors: active` rather than fighting it.
- **Hit targets.** `--hit-target-min` is 24px, and the chip remove button fills
  it despite a 10px glyph.
- **Reflow.** Measured in a headless Chromium at a **320px container**, by
  walking every descendant and comparing its right edge with the container's:
  `.df-headerbar` carrying a four-step `.df-stepper`, `.df-boxed-list`,
  `.df-toast`, `.df-state__actions` and `.df-banner` all reflow with **0px**
  overflow. The one exception is `.df-table--fixed` with five columns, which
  needs 400px — see §6.13.
- **Gating.** `aria-disabled` is styled like `:disabled` and has its hover and
  press states suppressed, but is never given `pointer-events: none` and never
  leaves the tab order. §6.23.
- **Layering.** `.df-popover` and `.df-stepper__reason` are positioned, not
  inline, so nothing transient reflows the virtualised list under it.

The numbers above are static properties of the token layer. What the *running
app* does with them — keyboard traversal of every screen, focus order, focus
restoration across modals, the virtualised list's `aria-activedescendant`, and
the accessibility tree the engine actually publishes — is measured and recorded
separately in `../../../docs/accessibility.md`, including what that pass found
and what it could not test.

---

## 10. Decisions a reviewer should second-guess

Listed because they were judgement calls, not because they are wrong.

1. **Two tokens per role (`--color-<role>` vs `--color-<role>-text`).** It is
   25 role tokens rather than about 12, and a screen author can pick the wrong
   one. The alternative — one colour per role — cannot satisfy both the 3:1
   icon floor and the 4.5:1 text floor while leaving enough luminance range for
   the CVD separation. I judged the extra tokens worth it; a reviewer may prefer
   fewer tokens and a looser CVD target.
2. **Modern colors with explicit compatibility palettes.** `light-dark()` is
   used on capable engines; §3 records why actual 2.36 captures required the
   matching explicit fallback. Palette edits must update and compare all
   fallback arms. `@layer` and `:has()` stay out on style grounds (§1).
   A reviewer may also revisit the
   `--shadow-1-dark`…`-3-dark` pairs (§3): `light-dark()` cannot switch a
   length, so the elevation set keeps a two-rule override, and a reviewer who
   preferred one shape everywhere might rather unify the dark shadow geometry
   with the light — which would change what the app renders, and so is a design
   change, not a refactor.
3. **px type sizes rather than rem.** Chosen so a user's browser font-size
   setting cannot desynchronise text from the fixed `--row-height`. The cost is
   that the app ignores that OS/browser preference and relies on webview zoom
   instead. Reasonable for a desktop tool; wrong for a web page.
4. **`--row-height: 32px`.** Dense — deliberately, for a 70,000-row catalogue on
   a builder workstation. If usability testing says it is too tight, changing it
   requires touching the virtualiser at the same time.
5. **Forced-state classes (`.is-hover`, `.is-pressed`, `.is-focused`) shipped in
   the library rather than confined to the gallery.** They make the gallery
   reviewable without a pointer and give the virtual list a keyboard cursor, but
   they can be misused to fake a state that assistive technology never hears
   about. Documented, but worth a second opinion.
6. **`--color-border-subtle` below 3:1.** Justified under WCAG 1.4.11 as
   decorative, but if a screen ever leans on it to bound a control, that is a
   real failure. Worth a lint rule.
7. **Tritanopia info/success convergence** — §8. Accepted deliberately; a
   reviewer may want a teal-shifted success instead, at the cost of looking less
   like a conventional "green means good".
8. **The gallery is generated, not hand-written.** The 200-row list and the
   swatch tables (including their measured ratios, read from `tokens.css`) come
   from a script, so the numbers cannot drift from the CSS. The script itself is
   not committed — only its output. If the palette changes, the gallery's swatch
   ratios must be recomputed by hand or the script rebuilt.
9. **`--color-brand-surface` is lighter than it wants to be.** A warmer, deeper
   sand looks better as chrome. It was pinned at the lightest value where
   `--color-warning` still clears 3:1 on it (3.06:1), so that a status icon can
   appear in the headerbar. A reviewer might prefer the deeper sand plus a rule
   forbidding status colours on chrome; I judged a rule nobody can see at review
   time to be the weaker guarantee.
10. **Semantic role colours are measured against `--color-brand-surface` and
    `--color-brand-canvas` only**, not against `--color-brand-hover`/`-active`.
    The argument is that a status icon sits on resting chrome and never inside a
    hovered ghost button. It is a scoping argument, not a proof — if a screen
    ever puts a `.df-status` inside a hoverable control on brand chrome, that
    pair is unmeasured.
11. **`.df-brand` re-points `--color-text-disabled`.** It makes a disabled
    control on brand chrome no dimmer than a secondary one. It is defensible
    only because `disabled` is banned there (§6.23); if that rule slips, this
    hides the state.
12. **The CVD numbers moved when the simulation was re-derived** (§8.1). They
    are close and support the same conclusions, but two published sets of
    numbers for one palette is a smell. A reviewer should decide which
    implementation is canonical and delete the other.
13. **`.df-stepper` has no roving tabindex.** Four steps as four tab stops is
    right for four steps and wrong for twelve. If the flow ever grows, this
    needs revisiting together with `docs/accessibility.md` §10 P3-13.
14. **`.df-boxed-list__row` uses `min-height: --row-height-comfortable` (44px).**
    GNOME's preference rows are taller still. It is denser than libadwaita on
    purpose, matching the rest of this system, and a reviewer may want the extra
    breathing room back.
15. **How far the `prefers-contrast: more` block goes** (§7.4). It promotes the
    two hairlines that are below 3:1 and thickens the ring, and stops. The
    argument is that there is nothing else below a floor to fix, so anything
    further would be a taste judgement dressed as an accessibility fix, and this
    file's whole method is to prefer a measured floor to a preference. A
    reviewer may reasonably hold that a user who turns on high contrast is
    asking for more than "meets the floor" — a black-on-white palette, or at
    least a promoted `--color-text-secondary` — and that the design system is
    hiding behind its own metric. The honest counter is only that the
    alternative has no number to check it against; if someone produces one, the
    block should grow. What is *not* negotiable is that a wider block would have
    to re-run §8's colour-vision solve, because it would introduce colours.
16. **`forced-colors` is kept although it is dead on the shipping engine**
    (§7.4, `docs/accessibility.md` §11.6). About 60 lines across two files run
    on Windows/WebView2 only, and nothing in this repository's CI exercises
    Windows. A reviewer might prefer to delete code no test can reach. I judged
    a supported platform's high-contrast support to be worth more than the
    tidiness, and made both blocks say plainly which platform they are for so
    the next reader is not misled the way the last one was.

---

## 11. What the brand pass changed, and what a consumer must do

This pass is the vocabulary for the UI redesign. Two product decisions are
settled and are not up for renegotiation here: the flow is a **gated stepper
with free navigation** (not a Next/Back wizard — the picker is a workspace people
revise for twenty minutes), and the palette is **identity-only Debark warmth**
(logo, wordmark, headerbar, about screen; working surfaces keep a measured,
table-safe interactive colour).

### New

`.df-groups` `.df-group` `.df-group__title` `.df-group__description`
`.df-boxed-list` (+ `__row` `__text` `__label` `__description` `__control`
`__control--field` `__chevron` `__slab` `__footer`) · `.df-stepper`
(+ `__list` `__item` `__step` `__marker` `__glyph` `__label` `__reason`
`__reason--end`, `--compact`) · `.df-toast-host` `.df-toast`
(+ `__icon` `__text` `__action` `__close`, `--info/--success/--warning/--danger`)
· `.df-popover-anchor` `.df-popover` (+ `__title` `__text` `__footer`,
`--end/--above/--flush/--wide`) `.df-help__btn` · `.df-app__skip`
`.df-app__banners` `.df-app__main` `.df-app__pane` `.df-app__content`
(`--wide`, `--bleed`) · `.df-brand` `.df-headerbar--brand` `.df-brand-mark`
(`--lg`) `.df-wordmark` (`--lg`) `.df-about` (+ `__version` `__text` `__rule`) ·
`.df-live` `.df-gate-reason` · `.df-panel--raised`

### Changed

- `--control-height-sm` / `--control-height` / `--control-height-lg`:
  24/30/36 → **26/34/40px**. `.df-toolbar`'s vertical padding drops to
  `--space-1` so `--toolbar-height` can stay 44px.
- `.df-headerbar`: `height` → `min-height`, and it now wraps.
- `.df-panel`: unchanged visually, **reclassified** — floating surfaces only.
- `.df-table--fixed`: real column rules, so more than one label column works.
- `.df-dialog__footer`: wraps.
- `.df-btn` hover and press: now excluded on `[aria-disabled="true"]`.
- `.df-app`: `position: relative`.
- Focus ring registered for `.df-stepper__step`, `.df-boxed-list__row` and
  `.df-help__btn`; `.df-boxed-list__row` draws it **inset**.
- `tokens.css` answers **`prefers-contrast: more`** (§7.4), which is the query
  Linux signals under GTK's HighContrast theme. `forced-colors` is now labelled
  in both files as Windows/WebView2 only, because it never matches on
  WebKitGTK — measured, not assumed.

### Deleted

- `--font-size-2xl` (23px) and `--font-size-3xl` (28px). Referenced by nothing
  but `gallery.html`; they were an invitation to put a marketing headline inside
  a tool window. **19px is the top of the scale.**

### Things a consumer must not do

1. **Do not repaint `--color-accent` with brand amber**, and do not use
   `--color-brand-*` outside `.df-brand`, `.df-about` or the logo mark. §8.2 is
   why: brand amber and `--color-warning` are ΔE 12 apart.
2. **Do not put `Inter`, `Space Grotesk` or any brand face ahead of `system-ui`**,
   and do not add a `--font-family-display`. Do not fetch a webfont.
3. **Do not stack `.df-panel` cards to group things.** `screens/build.js` has
   seven; they are the redesign's first job.
4. **Do not use `disabled` on a gated action or a locked step.**
5. **Do not put more than one `.df-toast` in a `.df-toast-host`**, and do not
   move the host to the bottom right.
6. **Do not remove `.df-app-host` from `index.html`**, or put the frame in an
   element without a resolved height. It is the difference between 25 rendered
   rows and 60,000.
7. **Do not put an inline `<details>` above the virtualised list.** Use
   `.df-popover`.
8. **Do not touch `--row-height`** (32px). It is the virtualiser's contract.
   `.df-boxed-list__row` is a different thing and has no relationship to it.
9. **Do not add a styled scrollbar, a cursor override or a `::selection`
   override.** There is none today; that is deliberate and native.
10. **Do not "fix" the radius scale (3/5/8px), the motion durations
    (80/140/240ms) or the focus ring.** They are already period-correct GTK.
11. If you change any surface or text colour, **re-measure every pair against
    it** and update §7. That is what §7.3 is for.

### Still open

- **Nothing carries a running build's progress to a screen reader on Linux.**
  Live regions are inert (§6.23) and a `role="progressbar"` announces exactly
  one value change and then goes quiet (§6.7). The design system has no third
  mechanism to offer: the only push channel left is moving focus, and moving
  focus during a run would yank an operator out of whatever they were reading.
  What it *can* do is make the state readable on demand, which §6.23's
  `aria-describedby` pull channel does. Not a defect in this library, and
  recorded so nobody writes "the build reports its progress accessibly".
- **Shell and tray private CSS: closed during desktop UX integration.** The
  shell uses the shared toolbar/layout/toast rules, and tray styling lives in
  `components.css`. Screen code does not inject those styles.
- **~~Nothing here has run on WebKit2GTK.~~ Closed during the third round.** Every
  measurement in §7 and §8 is arithmetic on the token values and is
  engine-independent; the render and reflow checks were taken in headless
  Chromium 152, which is the WebView2 target, not the shipping one. Since then
  `gallery.html` and the real binary have both been driven on WebKit2GTK 2.52.6
  — the OS media queries in `docs/accessibility.md` §8.3, and the whole token
  layer in both themes for the `light-dark()` collapse (§3). Three of the four
  old-fashioned exclusions remain by choice rather than necessity, and §1 says
  which and why.
- **~~Zoom to 200% is still untested~~ (WCAG 1.4.4).** Closed in the third
  round and the `--row-height` worry was unfounded: page zoom scales the CSS
  pixel, so a px-valued row height scales with everything in it and the
  virtualiser simply renders fewer, larger rows — 0 of 39, 0 of 21 and 0 of 17 rows clipped at
  1400@100%, 1400@200% and 640@200%. `docs/accessibility.md` §8.2. What is
  still untested is the OS *font-size* preference, which this system
  deliberately does not follow (§10.3).
- **A high-contrast palette, as opposed to a high-contrast floor** — §10.15.
