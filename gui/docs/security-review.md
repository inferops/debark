# Security review — the desktop GUI

> The capture journals, screenshots and raw logs cited in this document are
> part of the project's internal records and are not published with the
> source. The measurements they back are reproduced here in full.

**Current desktop simplification evidence:** the
[2026-09-07 hostile UI appendix](#desktop-simplification-hostile-ui-audit-2026-09-07)
records the revised harness and its three actual Chrome runs. The original
review and remediation history below remain evidence for their stated trees;
their method counts and browser coverage are not current acceptance claims.

First security review of `debark-gui`, code as read on **2026-09-06,
17:55 – 19:35 PDT (UTC−7)**, against commit `46ec3ca` and then against the
second-round tree as it moved under the review. The last other commit read was
`b184c39`; three screens were modified in the working tree at the close.
It follows the engine's discipline and format
(`../docs/security/review-2026-09.md`), and it should be read
alongside that record rather than instead of it: the two halves of this
product share a threat model and only one of them holds a signing key.

**The tree was under active repair throughout, and moved a lot.** Other packages
committed six times during this review, including the two cache correctness
items from that round's backlog (`9c64dc8`, `0b76ef2`, `b184c39`) and the
end-to-end run (`8fefa7d`). At the closing timestamp `git status` showed seven
modified files, three of them screens. Every finding below was re-read against
the tree at that timestamp; where a fix had already landed it is recorded as
landed, with the code that does it cited, and the rendering pass in §2 was
**re-run against the changed screens** and produced the same result. **Line
numbers will have moved**; treat them as a pointer to the right function.

**What this review could execute, and what it could only read.** The
rendering half was executed: the real screens were driven by a real browser
against fixtures built to break them, and the results in §2 are observations,
not arguments. The cache parser was executed: 795 million fuzz executions
across four targets. Rule 3 is now executed on every `go test` run. The
`.deb` could not be executed because it does not exist yet, so §5 is a
checklist for whoever builds it rather than a finding about one.

---

## 1. Executive summary

An operator or auditor can read this section alone.

> **Update, appended after remediation.** All four findings this section calls
> out by number — §3.1, §3.2, §3.3 and §4.4 — are now **fixed**, and the
> largest gap §8 named against this review is closed. §9 records what was done,
> what was verified by running rather than by reading, and the one defect the
> new fuzzing found *in* a fix. Nothing below is rewritten: it is what was true
> when the review was taken.

**Nothing rated Critical or High was found.** That is a real result rather than
a polite one, and the reason is architectural: this application resolves
nothing, verifies nothing, and installs nothing. It shells out to `debark`
for every decision that matters, so the trust boundaries that carry
`debark`'s Critical findings — a captured `apt.conf` fragment, the content
store, the signature block — are not on this side of the line at all.

Eight findings are recorded, the most serious two Medium, and both are in the
same place: the catalogue's index fetcher, which is the only code here that
opens a socket.

- **§3.1 — the catalogue will fetch from any URI scheme a target's sources
  name, including an internal `http://` host.** A snapshot is untrusted input
  by construction; opening one causes a GET to every archive URI it names,
  before the operator has done anything else and with no signature check
  anywhere on the path. Medium, confirmed by execution.
- **§3.2 — `packages.go` has no decompressed-size cap.** Its sibling
  `dep11.go` caps an expanded index at 512 MB; the `Packages` path caps only
  the *compressed* body at 256 MB and then streams the inflated bytes into an
  unbounded slice of entries. Medium, confirmed by reading; the asymmetry
  between the two loaders is what makes it a defect rather than a design.

The other six are Low or informational: an HTTP client that follows redirects
to any host (§3.3), `meta.json` accepting a negative `entry_count` (§4.4), the
command preview rendering vendor-URL credentials verbatim next to a Copy
button (§6.1), a `file://` URL built by string concatenation (§6.2), a vendor
URL containing `=` misbinding its `--digest` entry (§6.5), and a committed
Windows installer that elevates to administrator and silently runs Microsoft's
online WebView2 bootstrapper (§6.6). §6.5 is the only one that can silently
weaken a check the operator believes is in force, and it is the cheapest to
fix. §6.6 is Low only because nothing ships from that script yet.

**What was verified sound matters as much as what was not.**

- **Rendering untrusted text holds, under fixtures built to break it.** The
  whole tree contains exactly one `innerHTML` assignment, it is fed from a
  frozen six-entry icon table through a normalising gate, and no other
  markup-producing sink exists. Three runs of a hostile fixture that poisons
  every one of the 300-odd strings the Wails bridge can carry produced zero
  injected elements, zero `on*` attributes, zero script executions and zero
  off-origin requests, across all five screens and 28 of the 49 bound methods.
  The payloads came back as *text*, verbatim, which is the positive evidence
  that they were escaped rather than silently dropped (§2).
- **Rule 3 is now proven by a test rather than asserted.** `internal/audit`
  fails if any source file names a debark-operated host, if any file outside
  a seven-entry inventory imports a network package, if any request
  destination is a compiled-in literal or constant, if the readiness check's
  archive host gains a default, or if the frontend acquires `fetch`,
  `XMLHttpRequest`, `WebSocket`, `EventSource`, `sendBeacon`, a worker, a
  remote subresource, an `@import` or a web font. It has none of those (§4).
- **Rule 4 holds and is measured**: the binary's link closure is 327 packages
  and none of them matches any of 40 telemetry, analytics, crash-reporting or
  session-replay markers.
- **Rule 5 holds**: one `package.json` in the tree, Wails' own generated
  runtime metadata, declaring no dependencies of any kind; no lockfile, no
  `node_modules`, no bundler config.
- **The cache parser survived nearly everything thrown at it**: 795,386,077
  executions of the existing `FuzzLoad` in 30 minutes — on top of the 148 M
  behind it — plus three new structured targets and a fifteen-case hostile
  image table. No panic, no hang, no unbounded allocation, no silent wrong
  answer. Three runs stopped early with inputs that do **not** reproduce; the
  captured message shows a worker process dying under machine load, not an
  assertion, and halving the worker count removed it entirely. §4.1a records
  the evidence rather than the guess.
- **Nothing in this application can be argv-injected.** Every `debark`
  invocation is built by `cliadapter.BuildArgv` as a vector, never a shell
  string, and every positional input goes behind `--` with an explicit
  `apt:`/`url:`/`file:` prefix so a value cannot be reclassified by its shape.
  `internal/readiness`'s `ExecRunner` is the same shape. There is no `sh -c`
  anywhere in the tree.
- **The cache key cannot escape the cache root.** `LoadCacheDir` deletes a
  directory on corruption, and that directory's name comes from a snapshot's
  own `os-release` fields, so this was checked specifically: `safeKeyPart`
  reduces each part to `[a-z0-9._-]` and then trims `.` and `-`, so a part of
  `..` collapses to the empty string and is dropped, and no part can contain a
  separator. No traversal is reachable (§4.5).

---

## 2. Rendering untrusted text

Package descriptions, file paths, volume labels, `debark`'s stderr and
vendor URLs all reach the DOM. Every screen's header comment claims it never
assigns `innerHTML`. This section is what turns that claim into a result.

### 2.1 The sink inventory — verified mechanically

**Status: sound. Now pinned by `internal/audit/domsinks_test.go`.**

A grep for `innerHTML` alone is not an audit; most of the ways a string
becomes markup are not called `innerHTML`. Fourteen sinks were enumerated and
scanned for across `frontend/`, with comments blanked so a screen's own
promise not to use one does not read as a use of it:

| Sink | Occurrences in shipped frontend |
|---|---|
| `innerHTML =` | **1** — `src/shell/shell.js`, reviewed below |
| `outerHTML =` | 0 |
| `insertAdjacentHTML` | 0 |
| `document.write` / `writeln` | 0 |
| `new DOMParser` / `parseFromString` | 0 |
| `createContextualFragment` | 0 |
| `iframe.srcdoc =` | 0 |
| `eval`, `new Function` | 0 |
| `setTimeout`/`setInterval` with a string body | 0 |
| `setAttribute('href'/'src'/'style'/'srcdoc'/'action'/'data')` | 0 |
| `.href =` / `.src =` / `.action =` | **3** — all reviewed below |
| `style.cssText =` | 0 |

The one `innerHTML` is `svg()` in `frontend/src/shell/shell.js:143`:

```js
function svg(markup, className) {
  const holder = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  …
  holder.innerHTML = markup;
```

All seven call sites were read. Five pass a literal member of the frozen
`ICON` table (`ICON.back`, `ICON.close`, `ICON.danger`); two pass
`ICON[normaliseKind(k)]`, and `normaliseKind` maps anything outside the
four-member `TOAST_KINDS` set to `'info'`. No value derived from the backend
can reach it. This is the correct shape for the pattern — a constant table
behind a total function — and the reason it is worth recording rather than
waving through is that it is one refactor away from not being: a future
`svg(spec.icon)` would look identical at the call site.

The three URL-bearing assignments:

- `shell.js:426` — `skip.href = '#shell-content'`, a literal fragment.
- `main.js:48` — `link.href` is the resolved URL of one of two literal,
  module-relative stylesheet paths. `addStylesheet` has no other caller.
- `picker-tray.js:1249` — `a.href = safeHref(items[0].doc_url)`. `safeHref`
  (`picker-tray.js:230`) parses with `new URL()` and returns `null` for
  anything whose protocol is not `http:` or `https:`. This is the only place
  operator- or backend-supplied text becomes a URL, and §2.3 confirms by
  execution that the refusal is real.

The other 60-odd `setAttribute` calls in the tree set `viewBox`, `stroke`,
`aria-*`, `role`, `title`, `for`, `placeholder` and `open` — none of which can
carry markup or a URL. Every `style` write is a literal or a clamped
percentage; `build.js:93`'s generic `node.style.setProperty(k, v)` is only
ever reached with token literals.

### 2.2 The hostile fixtures

Four fixtures were asked for. Rather than write four, the fixture generator
reads the Wails-generated bridge description — 51 view classes, 49 bound
methods — and poisons **every string field the bridge can carry**, because a
hand-written fixture poisons the four fields its author thought of. The
harness then installs it as `window.go.app.App`, boots the **real** shell, and
walks all five screens.

It is committed at `internal/audit/xssharness/`, with a README, so this
section can be re-run rather than believed:

```sh
cd internal/audit/xssharness && node genfixture.mjs && node run.mjs realistic
```

Node's `node:` builtins and Chrome; no npm packages, and nothing written into
the repository (Chrome's profile goes to the OS temp directory and is removed
on exit).

| # | Fixture | Payload | What happened |
|---|---|---|---|
| 1 | Package `Description` / summary / name | `<img src=x onerror="…">DESCRIPTION_PAYLOAD<svg/onload="…">` | Rendered verbatim as text. No `IMG`, no `SVG` outside the app's own icon set, `onerror` never fired. |
| 2 | Volume label | `MY USB" onmouseover="…" x='<b>LABEL_PAYLOAD</b>` | Rendered verbatim as text. No `B` element; no `onmouseover` attribute anywhere in the tree. |
| 3 | `debark` stderr / details / log lines | `</script><script>…</script>` then `</textarea></title></style><img src=x onerror=…>` | Rendered verbatim as text inside `<pre class="df-log">`. `#app script` count: 0. |
| 4 | Vendor URL and `doc_url` | `javascript:window.__xss("url-js")//https://vendor.example/a.deb` | **No anchor was created.** `safeHref` returned null. The payload appears only as text. |
| 5 | File paths, destinations, devices | `/mnt/<iframe src="javascript:…"></iframe>/bundle` | Rendered verbatim as text. No `IFRAME`. |
| 6 | CSS injection through style-ish fields | `red; background-image: url("http://127.0.0.1:7299/css-beacon.png")` | Never reached a style sink; no off-origin resource load. |
| 7 | Off-origin `<img src>` beacon (`icon_ref`) | `http://127.0.0.1:7299/src-beacon.png` | No `IMG` element was ever created; `PackageIcon` is not implemented. |
| 8 | Structural noise | bidi override `U+202E`, a 4,000-character run | Rendered as text; no layout break that hid a control. |

Detection was five independent signals, because any one of them can be
defeated by a payload that does not fit its shape: a global canary the
payloads call; any element in the mounted tree outside the app's own tag set;
any `on*` attribute; any resolved anchor protocol that is not http(s); and any
resource load or JS-initiated request that is not the harness's own origin.
The fifth signal is the positive one — the payload must appear **verbatim in
`textContent`** — which is what distinguishes "escaped" from "silently
dropped", and four of the eight payloads were confirmed present that way.

Each screen was scanned twice: once after mount and after firing all thirteen
typed hostile events, and again after opening every `<details>` and `<dialog>`
and clicking every in-screen control, which is what reaches the file-pick
results, the snapshot inspector and the tray's dialogs.

**Result, all three modes: 0 problems.** The `realistic` run was repeated at
the end of the review, after another package had modified `build.js`,
`export.js` and `picker-list.js`, and produced the same result — same 28
bindings driven, same four payloads present verbatim as text, same zero.

```
MODE realistic (enum fields legal, every free-text field poisoned)
  PAYLOADS RENDERED VERBATIM AS TEXT: desc,label,stderr,path
  ANCHORS: #shell-content
  XSS HITS: none
  SCRIPT ELEMENTS IN #app: 0
  BINDINGS CALLED: 28 of 49
  OFF-ORIGIN RESOURCE LOADS: none
  TOTAL PROBLEMS: 0

MODE poison (every string field poisoned, enums included)
  … TOTAL PROBLEMS: 0

MODE control (the same fields carry a legitimate https URL)
  ANCHORS: #shell-content | https://vendor.example/pool/CONTROL_URL.deb
  … TOTAL PROBLEMS: 0
```

### 2.3 Why the control mode exists

The sibling review's §9.2 is emphatic that the first question to ask of a test
guarding a security property is whether the fixture can produce the failure at
all. A run in which the `javascript:` URL produced no anchor is
indistinguishable, from the outside, from a run in which that code path never
executed.

So the third mode feeds the **same fields** a legitimate `https://` URL. It
produced an anchor — `https://vendor.example/pool/CONTROL_URL.deb` — which
proves the `doc_url` → `<a href>` path is live, reached, and refusing on the
scheme rather than never running. Without that run, finding 4 above would have
been a claim about a code path nobody had entered.

### 2.4 What the browser run could not cover, and what covers it instead

Twenty-one of the 49 bound methods were never called: they need an interaction
sequence a harness cannot invent (`AddURLs` after typing into the tray's URL
dialog, `PlanExport` after choosing a volume, `StartVerify`, `BuildLog`
paging). A browser run therefore cannot be the whole answer, and its coverage
will silently shrink as screens grow.

`internal/audit/domsinks_test.go` is the half that does not rot: it fails on
the commit that introduces a new sink, whether or not anyone re-runs a browser
harness, and it requires every exception to carry a written reason. Its four
exceptions are the four sites in §2.1.

### 2.5 Reported to the frontend

Nothing here is a defect to fix. Two notes for whoever owns `frontend/` next:

- `picker-list.js:1484` computes `pct = Math.round(spec.progress * 100)` and
  writes `fill.style.width = pct + '%'` without the `Math.max(0, Math.min(100, …))`
  clamp that `export.js:197` and `export.js:1015` both apply. Not a security
  issue — a percentage cannot escape a CSS declaration written through the
  style property — but it is the one progress bar of three that can be told to
  be 4,000% wide.
- **If `PackageIcon` is ever implemented, it introduces the first off-origin
  subresource in the application.** Today it returns `IconResult{Found: false}`
  unconditionally (`bindings.go:1260`), which is why hostile fixture 7 could
  not produce an `<img>` no matter what `icon_ref` carried. An implementation
  would turn an `icon_ref` string taken from an archive's DEP-11 metadata into
  an image the webview fetches — a beacon that reports to the archive when the
  operator scrolled past a row, and a scheme-injection sink besides. The
  binding-surface backlog already records that the honest fix may be to delete
  the method (7.9 MB of icons for the 3% of rows DEP-11 covers, half of them
  fonts). That is also the answer that keeps `TestFrontendHasNoNetworkPrimitive`
  passing, and the two arguments should be decided together.
- `shell.js:606` and `:717` index `ICON[kind]` after `normaliseKind`, which is
  correct. Were the gate ever removed, `ICON['constructor']` would reach
  `innerHTML` as `"function Object() { [native code] }"` — inert, but it is
  the shape of the bug, and `Object.create(null)` or a `Map` for `ICON` would
  make the pattern safe by construction rather than by call-site discipline.

---

## 3. The catalogue's index fetcher

`internal/catalog/packages.go` and `internal/catalog/dep11.go` are the only
files in the application that open a socket. Everything below is about them.

The relevant context, from `docs/dev/catalogue-sourcing.md`: **the catalogue
is not a trust boundary.** It fetches indexes over HTTPS, verifies no
signatures, ships no keyrings, and a bug in it costs "a missing row in a
picker". That is a sound argument about *what the catalogue produces*. It is
not an argument about *what the catalogue reaches*, and the two findings below
are about the second thing.

### 3.1 — The catalogue fetches from any URI scheme, including an internal host

**Severity: Medium.**
**Confirmed by execution.**
**Status: FIXED — `f072e1b`, by the integration work. See §9.1, which also
records a second defect the fix's own fuzz target then found in the fix.**

**Where:** `internal/catalog/iface.go`, `Target.IndexRefs()` and
`IndexRef.URL()`; consumed by `PackagesLoader.packagesGet`
(`packages.go:471`) and `dep11Load` (`dep11.go:917`).

`IndexRefs()` filters a target's sources on three things: the stanza must be a
`deb` type, the URI must be non-empty, and the suite must not be flat. It does
**not** look at the scheme. `IndexRef.URL()` is then
`strings.TrimRight(r.URI, "/") + "/" + r.Path()`, and that string goes
straight into `http.NewRequestWithContext`.

Measured, by driving `IndexRefs()` with each scheme apt actually supports:

```text
file:/var/lib/mirror              -> file:/var/lib/mirror/dists/noble/main/binary-amd64/Packages.gz
file:///srv/mirror                -> file:///srv/mirror/dists/noble/…
cdrom:[Ubuntu 24.04]/             -> cdrom:[Ubuntu 24.04]/dists/noble/…
copy:/srv/x                       -> copy:/srv/x/dists/noble/…
mirror+file:/etc/apt/mirrors.txt  -> mirror+file:/etc/apt/mirrors.txt/dists/noble/…
ftp://archive.example/ubuntu      -> ftp://archive.example/ubuntu/dists/noble/…
http://10.0.0.1:8080/ubuntu       -> http://10.0.0.1:8080/ubuntu/dists/noble/…
```

Every one of those becomes a request. Two consequences, and they are of
different kinds.

**The local schemes are safe but produce the wrong error.** Go's default
`http.Transport` has no handler for `file:`, `cdrom:`, `copy:` or
`mirror+file:`, so `client.Do` returns *unsupported protocol scheme* and no
local file is ever read. But a target that legitimately carries a `cdrom:` or
`file:` source — an offline mirror, an install-media source, which is exactly
the population this product serves — gets a network-shaped failure instead of
a recorded `SourceProblem` saying "this source is not fetchable over HTTP, so
its packages are not in the catalogue". `catalogue-sourcing.md` names the
silently incomplete catalogue as the one unacceptable failure mode here, and
this is a loudly *wrong* one.

**The `http://` case is a request to a host the operator did not choose.** A
snapshot is untrusted input by construction — a file produced on another
machine and carried across the gap. Its captured `sources.list` is attacker
content. Selecting that snapshot resolves the target and builds the catalogue,
which issues a GET to every archive URI it names, from the builder machine,
before the operator has done anything except open a file, and with no
signature check anywhere on the path. Response bodies are size-capped and
parsed as deb822; anything that parses appears in the picker, so an internal
service whose response happens to contain `Package:` lines reflects field
values into the UI.

**Scoping this honestly.** `debark build` would eventually fetch from the
same URIs, so the *set of hosts* is not new — apt would reach them too. Three
things make it worth recording anyway: apt applies `apt-secure` and this does
not; apt runs at build time after the operator has committed to a target,
while this runs on target selection; and rule 3's whole posture is that this
application does not open sockets nobody asked for.

**Recommendation, for whoever owns `internal/catalog`:**

1. Add a scheme allow-list — `http` and `https` only — in `IndexRefs()`, and
   record everything else as a `SourceProblem` with its own kind, so the UI can
   say "this target has 2 sources the catalogue cannot browse" instead of
   failing a fetch. `SourceProblemKind` already exists and already has a
   rendering path.
2. Consider whether the first index fetch for a *snapshot* target should
   require the same confirmation the build does. This is a product decision,
   not a patch, which is why it is a recommendation and not a defect.

### 3.2 — `packages.go` has no decompressed-size cap; `dep11.go` does

**Severity: Medium.**
**Confirmed by reading when this was written; DEMONSTRATED since** — see
§9.2, which measures a real bomb's expansion ratio rather than estimating it.
**Status: FIXED — `73e82da`, by the integration work.**

**Where:** `internal/catalog/packages.go`, `packagesParseBody` (~line 585).

```go
if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
    gz, err := gzip.NewReader(src)
    …
    src = gz
}
stats, err := ParsePackagesIndex(ctx, src, ref, emit)
```

The *compressed* body is capped at `packagesDefaultMaxIndexBytes` = 256 MB by
an `io.LimitReader` in `packagesGet`. Nothing caps the inflated stream.
`ParsePackagesIndex` bounds each *line* (`sc.Buffer(…, packagesScanBufferMax)`)
and bounds its pre-allocation (`packagesMaxPrealloc`), but not the number of
stanzas, and each stanza emits an `Entry` the caller keeps.

gzip reaches about 1000:1 on repetitive input, so 256 MB of accepted body is
~256 GB of `Package: p1\n\n` — hundreds of millions of entries into an
unbounded slice, on a machine whose idle-memory budget is 400 MB.

The reason this is a defect and not a considered trade-off is the asymmetry:
`dep11.go` already has exactly this cap, named, documented and configurable —
`dep11MaxDecompressedBytes = 512 << 20`, applied at `dep11.go:977` — for the
same threat on the same kind of file from the same kind of host. Two loaders
in one package disagreeing about whether a decompression bomb is worth
bounding means one of them is wrong, and it is not the one with the cap.

**Recommendation:** mirror dep11's shape. Add `MaxDecompressedBytes` to
`PackagesLoader`, wrap `src` in an `io.LimitReader` after the gzip reader, and
fail with the existing `PackagesFetchError` naming the ref. A cap on emitted
entries would be a second, cheaper backstop, and would also bound the honest
case where a mirror really does serve something enormous.

### 3.3 — Both HTTP clients follow redirects to any host

**Severity: Low.**
**Confirmed by reading.**
**Status: FIXED — `73e82da`, by the integration work, with one of the three
redirects deliberately still allowed. See §9.3.**

Neither `packagesDefaultClient` (`packages.go:547`) nor `dep11DefaultClient`
(`dep11.go:824`) sets `CheckRedirect`, so both take Go's default: follow up to
ten redirects, to any host, including `https:` → `http:`.

An archive — or anything on the path of the plain-`http` archives Ubuntu still
publishes — can therefore redirect an index fetch to an arbitrary host. There
is nothing to steal (no credentials, no cookies, no auth header), the response
is size-capped, and the result is only a picker row, so this is Low rather
than Medium. It is recorded because it widens §3.1 from "hosts the snapshot
names" to "hosts anything the snapshot names can name", and because a
`CheckRedirect` that refuses a cross-host or scheme-downgrading redirect is
four lines.

### 3.4 — What was checked here and is sound

- **Size limits are real and correct on the compressed path.** `packagesGet`
  checks `Content-Length` against the limit *and* reads through
  `io.LimitReader(…, limit+1)`, so a lying `Content-Length` does not defeat it.
- **`Accept-Encoding: identity` is set deliberately** so `Content-Length`
  stays meaningful and Go's transport does not silently inflate a second time.
- **`ProxyFromEnvironment`** is honoured by the DEP-11 client, which is
  correct behaviour on a corporate builder and not a leak: the proxy is the
  operator's own environment.
- **The User-Agent names the project** —
  `debark-gui-catalog/1 (+https://github.com/inferops/debark/gui)`. This
  is not a rule-3 violation: `github.com` is not a debark-operated host and
  the string is never used to build a request. It does tell every archive
  operator that the client is this tool, which is conventional for an index
  fetcher and is the reason a `+URL` in a UA exists. Recorded so that nobody
  later reads it as a phone-home, and so the rule-3 scanner's treatment of it
  (host-only matching, §4) is on the record.

---

## 4. Rule 3, rule 4, rule 5 — and the cache parser

### 4.1 The fuzzing that was run

Actual budgets and actual counts, from the run logs.

| Target | Budget | Executions | Rate | New interesting inputs | Result |
|---|---|---|---|---|---|
| `FuzzLoad` (existing) | 30 min | **795,386,077** | ~442 k/s | 111 (corpus 236) | no crashers |
| `FuzzSecCacheImage` (new) | 8 min | stopped at 4m13s | — | 14 | worker died — §4.1a |
| `FuzzSecCacheImage` (re-run) | 6 min | **8,364,085** | ~23 k/s | 25 (corpus 99) | no crashers |
| `FuzzSecCacheRecords` (new) | 8 min | **112,352,041** | ~230 k/s | 33 (corpus 39) | no crashers |
| `FuzzSecCacheMeta` (new) | 4 min | **391,709** | ~3.3 k/s | 146 (corpus 155) | **one finding, §4.4** |
| `FuzzLoad` re-run | 8 min | stopped at 118 s | — | 0 | worker died — §4.1a |
| `FuzzLoad` diagnostic (8 workers) | 6 min | stopped at 83 s | — | 0 | worker died, message captured |
| `FuzzLoad` diagnostic (2 workers) | 6 min | **7,688,429** | ~21 k/s | 0 (corpus 241) | clean, no stalls |

`FuzzSecCacheMeta`'s finding came from a hand-written seed rather than from
mutation, which is worth saying plainly: the fuzzing did not find §4.4, the
seed corpus did, and the four minutes of mutation on top of it found nothing
further. A seed that encodes a question ("what does this parser do with a
negative count?") is doing different work from a mutator, and both belong in
the same target.

The rates differ by two orders of magnitude and that is the point of the
structured targets rather than a defect in them. `FuzzLoad` reaches ~442 k
executions a second because almost every input is refused by the eight-byte
magic before a single byte is read. `FuzzSecCacheImage` reaches ~23 k because
every one of its inputs is well-formed enough to be CRC-checked, validated
section by section, and then walked accessor by accessor. Twenty thousand
inputs that reach the bounds checks are worth more than four hundred thousand
that stop at the magic.

The `FuzzLoad` re-run exists because the two cache correctness commits
(`9c64dc8` clipping interned values at their call sites, `0b76ef2` making
`internCount > cacheMaxInterned` an `ErrCacheCorrupt`) landed at 18:14 and
18:15 while the 30-minute run was starting, so that run's binary may predate
them by a minute. The re-run was built after both landed and is the
one that covers the fixed code. Both are reported rather than only the
convenient one.

### 4.1a Three runs stopped early, and none of them was a bug in the parser

Three fuzz runs stopped before their budget and wrote a "failing" input:
`FuzzSecCacheImage` after 4m13s, `FuzzLoad` after 118 s, and `FuzzLoad` again
after 83 s. **None of the three inputs reproduces.** Re-running each one
directly passes in 0.00 s, every time.

The first two were reported with their output truncated to the last four lines
— a process mistake on this package part, and the reason the third run was
made with the whole log captured. It says exactly what happened:

```text
--- FAIL: FuzzLoad (83.21s)
    fuzzing process hung or terminated unexpectedly while minimizing: EOF
    Failing input written to testdata/fuzz/FuzzLoad/ee9c744d8fcabed5
```

That is Go's fuzzing coordinator reporting that a **worker process died**, not
an assertion firing. The distinction matters and it is why the file it writes
must not be believed: an assertion failure is minimised and re-verified before
the input is recorded, whereas a dead worker causes the coordinator to write
whatever input was in flight, unverified. All three files were exactly that.

The diagnosis was confirmed by changing one thing:

| Run | Workers | Duration | Executions | `waiting for fuzzing process to terminate` | Result |
|---|---|---|---|---|---|
| `FuzzLoad` | default (one per core) | 83 s | 12.7 M | **13** | worker died |
| `FuzzLoad` | `-parallel 2` | 6 min, full budget | 7.7 M | **0** | clean |

Halving the worker count removed both the stalls and the failure. The
throughput figures tell the same story from the other side: the uncontended
30-minute run managed ~442 k executions a second, and the same target on the
same code managed ~21 k while four other packages were running Docker
containers, browsers and benchmarks on this machine.

**So this is recorded as an environmental result, not as a suspected defect in
`cache.go`.** The three unverified corpus files were deleted rather than
committed: leaving a file in `testdata/fuzz/` says "this input once failed",
and the evidence says it did not — the next person to see it would spend an
afternoon chasing a machine's memory pressure.

Two things follow that are worth acting on, and both are reported to
integration rather than fixed here:

- **`go test -fuzz` on a shared builder needs `-parallel` bounded.** Whatever
  runs this in CI should say so, or a green fuzz job will occasionally report a
  failure nobody can reproduce, which is the fastest way to teach a team to
  ignore it.
- **Two tests in `internal/catalog` are load-sensitive and fail under exactly
  this pressure**, which the same machine demonstrated during this review:
  `TestCacheReaderRacingWriter` (passes at `-count=1`, fails at `-count=3`
  with `load during a rebuild: catalog: not built for this target`) and
  `dep11_test.go`'s parse-budget assertion (12.0 s against a 10 s budget). The
  second is a performance budget asserted inside a unit test, and a budget
  measured on a busy runner measures the runner. Neither is a security defect;
  both will produce failures that get explained away, which is the habit that
  eventually explains away a real one.

### 4.2 Why three new targets were needed

`FuzzLoad` is well designed. It tries every input twice — raw, and with the
trailer's length and CRC recomputed — precisely because a mutated file fails
its checksum with overwhelming probability, and a fuzzer that stops at the CRC
would spend its whole budget proving that `crc32` works.

What it still leaves thin is everything *between* the CRC and the record
bounds. A repaired random buffer almost never has the eight-byte magic, the
right format version, `header_size == 64`, zero flags, the trailer magic
`DFGCEND\n`, the trailer's zero reserved word **and**
`records_len == entry_count × 72`, all at once — and every structural check in
`CacheFromBytes` sits behind that conjunction. The overwhelming majority of
executions are refused by the magic or by `records_len` long before the
section-extent arithmetic runs.

`internal/catalog/sec_fuzz_test.go` adds three targets that are structured
rather than raw:

- **`FuzzSecCacheImage`** builds a file that is always well-formed down to the
  trailer and lets the fuzzer choose only the five section words plus the
  section bytes. Every execution reaches the extent validation, the forward
  non-overlapping section order, the even-`catids_len` rule, the two counted
  sections' two-step bound, the interned-table ceiling and the arena's
  reserved NUL. Each execution runs twice: once with the fuzzer's
  `records_len` and once with it made consistent with `entry_count`, so the
  single comparison that refuses most headers cannot swallow the budget.
- **`FuzzSecCacheRecords`** rewrites one 72-byte record of a real cache and
  re-seals it, so every execution reaches the per-record reserved-bit check
  and, on acceptance, the point-of-use `(off, len)` validation that §5.1 of
  the format document requires of `Entry`, `Haystack` and the category ids.
- **`FuzzSecCacheMeta`** covers the other parser in the load path.
  `meta.json` is the commit marker: a directory whose meta is unreadable has
  no usable cache, and that decision is `encoding/json` over bytes on disk —
  the first attacker-adjacent bytes the loader touches.

`TestSecCacheHostileImages` adds fifteen named corner cases a fuzzer reaches
only by luck: an empty arena, a non-NUL arena byte 0, an interned table
declaring exactly the ceiling and one past it, counted sections claiming more
than the file holds, `records_off + records_len` wrapping, and every section
offset set to `^uint64(0)`.

### 4.3 What the fuzzing found: nothing new in `catalog.bin`

No panic, no hang, no unbounded allocation and no silently-wrong answer from
`CacheFromBytes` or any accessor over it. The loader's arithmetic is uniformly
`uint64` (`cacheSection` explicitly checks `end < off` for wrap), every counted
section is bounded in two steps, and every `(off, len)` pair is re-checked at
the point of use with the bad-reference counter the format document specifies.

Two things worth naming as *verified*, because they were the backlog items the
concurrent correctness work was fixing while this review ran:

- **`internCount > cacheMaxInterned` is now `ErrCacheCorrupt`** (`cache.go:593`
  in the working tree), with a comment that states the reason correctly: not
  memory-unsafe, because every id is range-checked where it is used, but a
  file declaring more than 65,535 interned strings files string 65,536 under
  id 0 — the id the format reserves for "absent" — so every row naming it
  silently loses the field inside a CRC-valid file. Both sides of the boundary
  are now covered by named cases in `TestSecCacheHostileImages`.
- **`Entry` drops an empty category name.** `secExerciseCache` asserts it
  directly, because an out-of-range interned id yields `""` and a blank chip
  in the picker is the visible form of exactly that silent-mislabelling
  failure.

### 4.4 — `ReadCacheMeta` accepts a negative `entry_count` and `bin_size`

**Severity: Low (defence in depth; not exploitable on any path in the tree).**
**Confirmed by execution — found by `FuzzSecCacheMeta` seed #6.**
**Status: FIXED — `bb05231`, by the integration work, along with the
`meta.BinSize != 0` inconsistency noted at the end of this finding. See §9.4.**

`{"format_version":1,"entry_count":-1,"bin_size":-1}` is accepted and returns
a `CacheMeta` carrying both negatives, so the struct can hold values that
contradict its own field comments ("EntryCount and BinSize describe
catalog.bin").

Nothing downstream is harmed today, and the reason is worth stating so nobody
raises the severity by mistake: `cacheLoadIndex` compares `meta.EntryCount`
against `cf.Len()` and `CacheReadyDir` compares `meta.BinSize` against the
file's real size. A negative matches neither, so the answer is "rebuild" —
which is the right answer.

It matters because the next caller might not compare. `make([]X,
meta.EntryCount)` panics on a negative; a progress denominator of `-1` renders
as nonsense. A structural check in `ReadCacheMeta` — reject a negative count
or size as `ErrCacheCorrupt`, the same as an unparseable file — costs two
lines and removes the class.

The fuzz target deliberately does **not** assert this, and says so in a
comment at the assertion: asserting a property the code does not have would
leave a red test in a package this package does not own.

**Related, noticed while tracing it:** `cacheLoadIndex` guards the size
cross-check with `meta.BinSize != 0 &&`, so a `meta.json` claiming `bin_size:
0` skips it entirely. `CacheReadyDir` has no such exemption. The two paths
disagree about whether zero means "unknown", and only one of them is reachable
without going through `Ready`. Informational.

### 4.5 The cache key cannot escape the cache root

Checked specifically, because `LoadCacheDir` calls `os.RemoveAll` on a
directory whose name derives from a snapshot's own `os-release`
(`DistroID`, `VersionID`, `Arch`) — attacker-influenced text reaching a
recursive delete is worth ten minutes of anyone's time.

`Target.CacheKey()` passes each part through `safeKeyPart`
(`iface.go:1036`), which keeps only `[a-z0-9._-]` — every other rune,
including `/` and `\`, becomes `-` — and then `strings.Trim(s, "-.")`. So:

- A part of `..` becomes `..` after filtering and `""` after the trim, and
  empty parts are dropped from the join.
- No part can contain a path separator, so the key is always one segment.
- The 12-hex identity prefix is always present, so the key is never empty.

`filepath.Join(root, key)` is therefore always a direct child of the cache
root. **No traversal is reachable.** Recorded as sound rather than left
unmentioned, because the next person to read `os.RemoveAll` in this file will
ask the same question.

### 4.6 Rule 3, mechanically — how the test works and how it fails

`internal/audit` is a package of nothing but checks. It reads the tree.

**Five tests, each failing for a different reason**, because no one of them is
sufficient:

1. **`TestNoDebarkOperatedHostAppearsInAnySource`** — every `.go`, `.js`,
   `.html`, `.css`, `.json`, `.yml`, `.md`, `.sh` and `.mod` file in the tree
   is scanned for two things: the host of any absolute URL, and a bare
   `debark.<tld>` domain. A host is "debark-operated" if any of its
   labels is `debark`, or it starts `debark-` / ends `-debark`. The test
   is on the **host**, never the path: `github.com/inferops/debark/gui` is a
   repository under GitHub's control and naming it is not a phone-home;
   `catalog.debark.io` is the thing rule 3 forbids.
   *Fails when* anyone adds a hostname under a project domain, in code, in a
   comment, in a config file or in a document. The exemption table is a
   `map[string]string` of token → reason, and it is **empty**.

2. **`TestOnlyInventoriedFilesTouchTheNetwork`** — every Go file's imports are
   parsed; any that imports `net`, `net/http`, `net/http/httptest`,
   `net/http/httputil`, `net/rpc`, `net/smtp`, `crypto/tls`,
   `golang.org/x/net/proxy` or `gorilla/websocket` must appear in a
   seven-entry table with a written reason. `net/url` is deliberately not on
   the list: it parses, it does not connect.
   **This is the test that catches a future phone-home**, and it is the reason
   the package exists. It does not matter which host such a commit names or
   how it spells it: it has to import a network package from a file that is
   not on the list.
   *Fails when* a new file imports one, and the failure message says that
   adding an entry is a design decision rather than a test fix. It also fails
   when a listed file stops importing one (a stale entry nobody will question
   later), while a listed file that has been *deleted* is logged rather than
   failed — a dead entry is not a hole, and failing on it would mean deleting
   a benchmark elsewhere breaks this package.

3. **`TestNoRequestURLIsCompiledIn`** — inside those files, every
   request-construction call (`http.Get/Head/Post/NewRequest*`, `net.Dial*`,
   `tls.Dial`, and the method forms `client.Get/Head/Post`,
   `dialer.Dial/DialContext`) has its destination argument examined. A string
   literal that looks like a destination fails. An identifier that resolves to
   a package-level string constant containing `://` fails, because a
   compiled-in endpoint with a name is still a compiled-in endpoint. The test
   also fails if it found fewer than four such sites, so a rename or a wrapper
   cannot turn it into a check of nothing.
   This is what makes "the only hosts are the ones the target's own sources
   name" a **structural** property: the destination is a runtime value, so
   there is nowhere for a compiled-in host to live.

4. **`TestArchiveHostHasNoCompiledInDefault`** — the readiness pack's
   `archive-network` check is the one place that dials a bare host. Its own
   comment argues it is acceptable because "the host is never this package's
   to pick": `Options.ArchiveHost` is empty unless the caller sets it, and the
   check is not registered when it is empty. This test pins that argument by
   failing on any non-test assignment of a literal to `ArchiveHost`. There is
   none — including in `internal/app`, which never sets it, so the check does
   not currently run at all. That is the safe direction and is reported to
   integration in §7 as a functional gap rather than a security one.

5. **`TestFrontendHasNoNetworkPrimitive`** — the webview half, which Go's link
   closure says nothing about. Fourteen patterns: `fetch`, `XMLHttpRequest`,
   `WebSocket`, `EventSource`, `sendBeacon`, service workers, `importScripts`,
   `new Worker`, a remote `<script src>`, a remote `<link href>`, a remote
   `<img>/<iframe>/<video>/<audio>/<source>/<embed>/<object>`, `@import`,
   `@font-face`, and a CSS `url()` carrying a scheme. **The frontend has none
   of them.** Comments are blanked before matching, because `tokens.css` opens
   by promising "no @import, no webfont, no network reference" and a check
   that fires on the sentence promising the property is a check nobody keeps.

**And a test about the tests.** `TestRule3ScannerDetectsAPlantedEndpoint` runs
every matcher over source that *does* violate the rule — four planted URLs
including one hiding the host behind `user:pw@`, three bare domains, and a
page that phones home six ways — and then over the tokens this tree
legitimately contains (`debark.events/v1`, `debark.theme`,
`https://github.com/inferops/debark/gui`) to confirm they do not match.
Fourteen patterns that quietly stopped matching would look exactly like a
clean tree.

The planted violations are assembled at run time from `"deb" + "ferry"`, so
the audit package is scanned by its own scanner rather than exempted from it.
A test file exempt from the rule it enforces is the obvious place to hide a
violation.

**What the test cannot prove**, stated in the package doc so a reader of a
green run knows its edges:

- It cannot see runtime behaviour. If an operator typed a vendor URL naming a
  debark-operated host, this would not notice — and should not: rule 3
  forbids *this application* choosing such a host, not the operator typing one.
- It cannot see into a dependency. Wails links an HTTP server for its own dev
  mode; the check is on module names in the link closure, not on what each
  module does. That judgement belongs in `docs/dependency-review.md`.
- It reads the source tree, not the shipped binary. A build injecting a host
  through `-ldflags -X` would pass. Recorded as a residual rather than papered
  over.

### 4.7 Rule 4 — telemetry

`go list -deps .` returns **327 packages**. None matches any of 40 markers for
telemetry, analytics, crash reporting or session replay (Sentry, Bugsnag,
Rollbar, Raygun, Honeybadger, Airbrake, Datadog, New Relic, Elastic APM,
Honeycomb, OpenTelemetry, OpenCensus, Prometheus client, statsd, Segment,
Amplitude, Mixpanel, PostHog, Heap, Google Analytics, Firebase, App Center,
Application Insights, Instana, Dynatrace, LogRocket, FullStory, Smartlook,
Countly, Matomo, Plausible, Crashlytics, Breakpad, Crashpad, …).

The link closure is the honest set, and the reason is worth keeping: the
module graph is much larger, because the Wails CLI pulls in a template engine,
a file watcher and an ANSI parser that never reach the binary. Auditing
`go.mod` would report packages nobody ships.

A second test scans **every** Go file's imports, including tests and
build-tagged files, because a library reached only from a test is still a
library in the tree — which is what rule 4 forbids.

The application's only diagnostic sink is `console.error`, and `shell.js`
says so at the call site: *"console is the only sink. There is no crash
reporting in this tree and there never will be — rule 4."*

### 4.8 Rule 5 — npm

One `package.json` in the tree: `frontend/wailsjs/runtime/package.json`,
Wails' own generated runtime metadata, committed so a fresh clone builds
without the `wails` CLI. It declares **no `dependencies`, `devDependencies`,
`peerDependencies` or `optionalDependencies`**.

No `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `npm-shrinkwrap.json`,
`bun.lockb` or `deno.lock`. No `node_modules`. No bundler or transpiler
configuration of any kind — the second test checks for thirteen of them,
because a build step is the change that makes an npm dependency possible, and
it is easier to spot before it has one than after.

`.gitignore` deliberately does not ignore `node_modules`, so one appearing
would show in `git status`. That is good practice and worth keeping.

---

## 5. The `.deb` must not ship anything that runs privileged

**The package does not exist yet.** This section is therefore a checklist for
the packaging work rather than a finding, and it is written so that each line is
testable against a built `.deb` with `dpkg-deb`, `find` and `lintian` — not as
advice.

The principle, and it is not negotiable: **this application needs no
privilege, at any point, for any operation.** It reads apt indexes over HTTPS,
writes to `$XDG_CACHE_HOME` and to directories the operator picked in a file
dialog, and shells out to `debark`. It never opens a block device, never
partitions or formats anything, never writes bootable media, and never
installs a package on the builder. `internal/export`'s own type comments say
so, and so does the permanent do-not-build list —
[`GOVERNANCE.md`](../../GOVERNANCE.md) for the project-wide one, and
`docs/dev/contract-brief.md` for this application's own entries, which name
writing bootable USBs and any raw block-device access explicitly. A `.deb` that
acquires root contradicts the product.

There is a second reason, stated in `internal/readiness/checks.go` and worth
repeating here: the audience for this tool runs locked-down machines, and *"a
tool that acquires root to help is the thing this audience will not install."*

### 5.1 The checklist

Each row is a command that must produce the stated result against the built
package. `$DEB` is the package file; `$ROOT` is `dpkg-deb -R $DEB` unpacked.

| # | Requirement | How to check | Must be |
|---|---|---|---|
| 1 | **No setuid or setgid file.** | `find $ROOT -perm /6000` | empty |
| 2 | **No file capabilities.** | `find $ROOT -type f -exec getcap {} +` | empty |
| 3 | **No maintainer script at all, if that is achievable.** | `ls $ROOT/DEBIAN/` | `control`, `md5sums` only |
| 4 | If a `postinst` is unavoidable, **it does exactly one thing** from: `update-desktop-database`, `gtk-update-icon-cache`, `update-mime-database`. Nothing else. No `chmod`, no `chown`, no `setcap`, no `usermod`, no `adduser`, no writing outside `/usr/share`. | read it | one command, guarded by `[ "$1" = configure ]` |
| 5 | **No `preinst`, no `prerm`, no `postrm` beyond `dpkg`'s own cleanup.** | `ls $ROOT/DEBIAN/` | absent |
| 6 | **No systemd unit, no init script, no user unit.** | `find $ROOT -path '*systemd*' -o -path '*init.d*'` | empty |
| 7 | **No polkit action or rule.** | `find $ROOT -path '*polkit-1*'` | empty |
| 8 | **No udev rule.** | `find $ROOT -path '*udev*'` | empty |
| 9 | **No D-Bus system-bus service or policy.** A session-bus file is acceptable only if something actually needs one; nothing does today. | `find $ROOT -path '*dbus-1/system*'` | empty |
| 10 | **No sudoers fragment.** | `find $ROOT -path '*sudoers*'` | empty |
| 11 | **No PAM, no `/etc/cron*`, no `/etc/profile.d`, no `/etc/ld.so.conf.d`.** | `find $ROOT/etc -type f` | only genuine config, if any |
| 12 | **No file installed outside** `/usr/bin`, `/usr/share/applications`, `/usr/share/icons`, `/usr/share/doc/debark-gui`, `/usr/share/man`, `/usr/share/metainfo`. | `dpkg-deb -c $DEB` | as listed |
| 13 | **Every file is mode 0644, every directory 0755, the binary 0755, all `root:root`.** | `dpkg-deb -c $DEB` | as stated |
| 14 | **No `Depends`, `Recommends` or `Suggests` on anything that grants privilege** — not `policykit-1`, not `pkexec`, not `sudo`. `libwebkit2gtk-4.1-0` and friends are normal library dependencies and are correct. | `dpkg-deb -f $DEB Depends Recommends Suggests` | libraries only |
| 15 | **The `.desktop` file's `Exec=` is the plain binary**, with no `pkexec`, no `sudo`, no `gksu`, no `Terminal=true` wrapper script. | read it | plain path |
| 16 | **No `libcap`, `libcap-ng` or `libpolkit` in `DT_NEEDED`; no capability symbol at all; and every `set[ug]id`-family import matched by a corresponding `_cgo_libc_<name>` wrapper.** An unmatched one fails the row. **Read §5.1a before running this** — the obvious form of this check fails on every possible build of this product. | `objdump -p` for `NEEDED`, `nm -D`/`objdump -T` for symbols, and `go list -deps .` | no privilege library; the nine cgo wrappers and nothing else |
| 17 | **Nothing is installed into the operator's home by the package.** The cache and config directories are created at run time by `os.UserCacheDir()`, owned by the operator. | `dpkg-deb -c $DEB` | no `/home`, no `/root` |
| 18 | **`lintian` reports no `setuid-binary`, `setgid-binary`, `elevated-privileges`, `maintainer-script-should-not-*`, or `privileged-*` tag.** | `lintian $DEB` | clean on that class |
| 19 | **Installing and removing the package changes no permission, group membership, or system service state.** | `dpkg -i` then `dpkg -r` in a container, diffing `getent group`, `systemctl list-unit-files`, and `find / -perm /6000` before and after | no diff |
| 20 | **The application starts and reaches its first screen as an unprivileged user with no `sudo` available.** | run it in a container as a non-root user with `sudo` removed | starts |

### 5.1a — Row 16's obvious form is a false failure, and it fired

**Written after the packaging work ran the checklist against a real `.deb`. Row
16 as originally worded failed, and the binary is fine.**

Every cgo Go binary on Linux imports nine credential-changing symbols —
`setuid`, `setgid`, `setgroups`, `setresuid`, `setresgid`, `setreuid`,
`setregid`, `seteuid`, `setegid` — because `runtime/cgo/linux_syscall.c`
compiles a `_cgo_libc_set*` wrapper for each of them so that Go's own
`syscall.Setuid` and friends can apply the change to every thread. They are
present whether or not any code calls them. The packaging work confirmed this
is not something about *this* binary by compiling a cgo hello-world as a control
and getting the identical nine.

The GUI links GTK and therefore can never be `CGO_ENABLED=0`, so it will always
have all nine. A row that fails on every build a product can possibly have is
worse than a row that looks in the wrong place: §5.2 says rows 1–18 are each
defeatable by a package doing the privileged thing somewhere the row did not
look, and that is a known limit. A row that always fails is a row that gets
routinely waived, and a waived row protects nothing.

So row 16 now checks the three things that actually discriminate:

1. **`DT_NEEDED` names no privilege library** — no `libcap`, no `libcap-ng`, no
   `libpolkit`. This is the one an attempt to acquire capability would need and
   the one the cgo runtime never brings in.
2. **No capability symbol at all** — `cap_set_proc`, `cap_from_text`,
   `capset`, `capget`. None of these has a cgo wrapper, so any occurrence is a
   real one.
3. **Every `set[ug]id`-family import is matched by its `_cgo_libc_<name>`
   wrapper.** A matched pair is the Go runtime. An *unmatched* import is code
   that called it, and fails the row.

On the `go list -deps` half: **358 packages link into the binary, exactly one
matches a capability grep, and it is `golang.org/x/sys/unix`'s generated
constant table, where `CAP_SYS_ADMIN = 0x15` is a number in a list.** No
package in the closure calls any credential-changing function.

Recorded here rather than only in the row because the next person to run row 16
will hit the same wall, and the two ways out of it — an hour spent rediscovering
`linux_syscall.c`, or waiving a row that should never be waived — are both worse
than a paragraph.

### 5.2 Two notes for the packaging work

- **Rows 19 and 20 are the ones that actually prove it.** Rows 1–18 are each
  defeatable by a package that does the privileged thing somewhere the row did
  not look; a before/after diff of a real install is not.
- **If any row cannot be satisfied, that is a design conversation, not a
  waiver.** The honest form of "this needs root" in this product is a
  documented manual step the operator performs themselves, not a maintainer
  script that performs it for them. The readiness screen already exists to
  tell an operator what is missing and what to do about it, and that is where
  such a requirement belongs.

---

## 6. Smaller findings

### 6.1 — The command preview shows vendor-URL credentials next to a Copy button

**Severity: Low.**
**Confirmed by reading.**
**Status: open. `frontend/` and `internal/app` belong to other packages.**
**Update, third round: the redaction now exists, as `cliadapter.RedactArgv`, and
§6.1a answers the question the finding turns on — what an operator is supposed
to do with the command they are shown. Two call sites in `internal/app` remain,
because that package was being written by another package; §6.1a names them
exactly.**

`newSelectionEntry` (`internal/app/bindings.go:3157`) validates a vendor URL by
prefix only — `strings.HasPrefix(v, "https://")` — so
`https://user:token@vendor.example/pool/agent.deb` is accepted, which is
correct: apt-style credentials in a URL are a real deployment and `debark`
is the thing that resolves it.

That URL then reaches `PreviewCommand`, and `build.js:300`'s `argvDisplay`
renders the argv verbatim, shell-quoted, in a `<pre>` with a **"Copy the
command"** button beside it — a control whose entire purpose is to put the
text somewhere else, which in practice is a bug report or a chat message.

The asymmetry is what makes it worth recording: `internal/cliadapter`'s
`errSafeURL` (`errors.go:401`) already does the right thing on the error path,
reducing a URL to host plus final path element and dropping userinfo, query
and fragment, with a comment explaining that "there is no reason for a summary
to carry any of them". The same reasoning applies to a string built to be
copied, and the preview does not apply it.

`debark`'s own review recorded the same shape as a finding
(credentials reaching `lock.json` warnings, fixed by `fetch.RedactURL`), which
is an argument for consistency across the two modules.

**Recommendation:** redact userinfo in the *displayed and copied* preview and
say so in the UI ("credentials in the URL are hidden here"), while passing the
real value to `debark`. Rule 8 wants the operator shown the command; it does
not want the token in their clipboard.

### 6.1a — What the command is for, and where the redaction went

**Status: the redactor and its tests are done and committed
(`internal/cliadapter/redact.go`). Two one-line call sites in `internal/app`
are not, and are listed below.**

**The question this turns on.** Rule 8 says the CLI stays the complete
interface and the GUI shows that command where it reasonably can. A redacted
command is not a runnable command; a command with a password in it is a password
on someone's screen and in their clipboard. Both halves of that are true, so the
finding cannot be settled by reading the rule. It is settled by asking what the
operator does with the string.

The preview has two audiences and they want different things:

- **Read it.** *"What is this application about to run on my machine?"* That is
  rule 8's actual purpose — the GUI must not be a black box — and a redacted
  command answers it completely. Every flag, every input, the exact shape, in
  the order it will be passed.
- **Run it.** Paste it into a shell, a script, a ticket, a chat message. Exactly
  one of those four is a safe home for a secret, and the application cannot tell
  which one the clipboard is going to. The review's own observation is that the
  button is used in practice for the other three.

A command carrying a password serves the second audience and endangers it. A
redacted command serves the first completely and the second partially: an
operator pasting into a terminal has to re-supply the credential, which is a
thing they can do — they typed it into this application, and a credential they
cannot reproduce is not one they should be building with.

**So the decision is: the redacted form is what is shown AND what is copied.**
There is no "reveal" control, and that is not a compromise avoided out of
laziness — a secret you can reveal is a secret on the screen, and the operator
already has the URL in the selection tray where they entered it. Nothing is
gained by putting it in a second place.

**Where the redaction lives, and why not in `Command`.** `Command` returns the
exact argv a build runs, and it has to: the real adapter execs
`Command(spec)[1:]`, and two tests exist specifically to pin that the previewed
command *is* the executed one (`TestInvokeCommandIsBuildArgv`, and the drift
check in `events_test.go`). Redacting there breaks builds outright; redacting in
`BuildArgv` breaks the same property more quietly, by making the preview a
description of a command nobody runs. So the exact argv stays exact, and
`RedactArgv` is a separate, explicit transform applied where the argv stops
being an instruction to a process and becomes text for a person.

That it is **one function rather than a rule each screen follows** is the part
worth insisting on. The core-engine work found, unplanned this round, that
`evidence.json` — hashed by the manifest and covered by the signature — was
carrying a vendor URL's username and query token verbatim, because `url.Error.Error()` prints the
request URL (core `034f709`). Same class of defect, in a place nobody had
thought of as a render site. A redaction applied at each place someone remembers
is a redaction that will be missed at the place nobody thought of.

**The redaction is `debark`'s own, not a second one.** `RedactArgv` calls
`fetch.RedactURL` from the core repository — already in this binary's link
closure, so it costs nothing — because the argument for fixing §6.1 at all was
consistency between the two halves of one product, and two redactors that can
disagree is two answers to one question.

Importing `core/fetch` does open one narrow hole, and it is closed in the same
change rather than left. §4.6's inventory lists every file allowed to import a
package that can open a socket, and `core/fetch` was not on the list of such
packages — so nothing had ever stopped a file here importing the fetcher and
calling `Fetch` rather than `RedactURL`. It is on that list now, with
`internal/cliadapter/redact.go` as its one permitted importer and the reason
recorded beside it. The inventory is a little stronger than before this change,
not weaker: it now guards a package it never watched.

It removes more than userinfo, deliberately: the whole query string goes too,
because a presigned URL's credential lives in a query parameter whose *name* is
not standardised (`X-Amz-Signature`, an Azure SAS `sig`, a bare vendor `token`),
so it cannot be told from a harmless one by pattern. **The cost lands here and
should be stated rather than discovered:** a vendor URL with an ordinary,
secret-free query string — a version pin, a mirror id — is redacted too, so for
that input the previewed command is a faithful description of the build and not
a paste-and-run script. That is the right trade for a string built to be copied,
and it is the same trade core made for `lock.json` and `evidence.json`. The
marker is visible (`REDACTED`), never a silent removal, so nobody reads the
result as a URL that never had a credential.

What it does, in one line each:

| Argv element | Becomes |
|---|---|
| `url:https://deploy:s3cr3t@vendor.example/pool/agent.deb` | `url:https://REDACTED@vendor.example/pool/agent.deb` |
| `url:https://cdn.example/agent.deb?token=abc&expires=99` | `url:https://cdn.example/agent.deb?REDACTED` |
| `--digest https://u:pw@vendor.example/a.deb=<sha>` | `--digest https://REDACTED@vendor.example/a.deb=<sha>` — the digest is not a secret and stays |
| everything else | untouched |

The `--digest` half splits at the **last** `=`, matching core's fixed flag
(§6.5a); for a spec that passed `Validate` the URL half holds no `=` at all, so
the two rules agree on every value this application emits. A `--digest` value
that is not a pair is redacted whole rather than passed through: the one thing
that must not happen in a redactor is a credential surviving because the string
was an unexpected shape. `RedactArgv` never modifies its argument — the caller's
slice is the one that gets executed, and a redactor that scrubbed in place would
turn a display concern into a build fetching `https://REDACTED@…`.

**What is left, in `internal/app` (the backend work's file while this was
written).** Three lines, and the third is the one that matters most:

1. `PreviewCommand` (`bindings.go`): `res.Argv` and `res.Display` should be
   built from `RedactArgv(argv)` rather than from `argv`.
2. `StartBuild` (`bindings.go`): `BuildStarted.Command` and
   `BuildStatus.Command` should carry `RedactArgv(argv)`. The argv actually
   passed to `Build` must stay the unredacted one — that is the string that
   runs. This is the pair the build screen renders and its details drawer
   copies, so redacting only the preview would fix the smaller of the two
   surfaces.
3. `PreviewCommand` does not call `spec.Validate()`, although `CommandPreview`
   already carries an `Error` field for exactly that and `StartBuild` does call
   it. So the preview will happily render a command the application would refuse
   to run — including, since §6.5a, a `--digest` pair that would silently
   misbind. One `if err := spec.Validate(); err != nil { res.Error =
   uiErrorFrom(err); return res }` closes it.

**And one line in `frontend/src/screens/build.js`.** The preview panel needs to
say that credentials in a URL are hidden, once the strings reaching it are
redacted. Adding that sentence *before* the backend redacts would be worse than
saying nothing, because it would claim a protection that is not there — so the
two changes go together, in that order.

That panel is not neutral about this today. Photographed on the running app
while validating §6.7a, its own copy reads *"debark is the whole interface;
this window is a way of driving it. Copy this line and you get the same bundle
from a terminal"*, and its actionbar says *"Ready. The command above is exactly
what will run."* Both sentences become false for a credentialed URL the moment
the redaction lands, and "exactly" is the word an operator would rely on. The
frontend edit is therefore not decoration on top of the backend change; it is
the half that keeps the screen honest.

Tests: `internal/cliadapter/redact_test.go`.
`TestRedactCommandLeaksTheCredentialToday` asserts the leak deliberately, so the
reason `RedactArgv` exists cannot quietly stop being true;
`TestRedactArgvRemovesEveryCredential` drives a spec carrying both a userinfo
password and a presigned query token through the real `BuildArgv` and checks
neither secret survives anywhere in the result, while everything that is not a
credential does.

### 6.2 — `RevealPath` builds a `file://` URL by string concatenation

**Severity: Low (correctness, with a small security edge).**
**Confirmed by reading.**

`internal/app/bindings.go:513`:

```go
wruntime.BrowserOpenURL(ctx, "file://"+filepath.ToSlash(abs))
```

No escaping. A path containing `#` truncates at the fragment, `?` at the
query, and a literal `%` is read as a percent-escape — so the wrong thing is
opened, or nothing is. On Windows the result is `file://C:/…`, which puts the
drive letter in the URL's *authority*, not its path.

There is no command injection here: Wails passes the URL to the platform
opener as an argument vector, not through a shell. The security edge is
narrower and worth one sentence: a UNC path becomes `file:////server/share/…`
and opening it can cause an SMB authentication attempt to whatever host the
path names. The operator chose the path, so this is Low.

**Recommendation:** build it with `(&url.URL{Scheme: "file", Path: abs}).String()`,
which escapes correctly and puts a Windows drive letter where it belongs.

### 6.3 — The export dereferences symbolic links by default, and nothing sets otherwise

**Severity: informational.**

`internal/export` has a deliberate, documented `SymlinkPolicy` with a
`SymlinkFail` option. `internal/app` never sets it, so every export runs with
`SymlinkDereference`: a link inside the bundle is copied as the *contents* of
its target.

This is not exploitable today, and the reason is on the other side of the
line: `debark`'s own `verify` refuses a bundle whose manifest-listed path is
not a regular file carrying its own bytes — a High finding fixed in the
engine during its second review — so a verified bundle contains no
links to dereference.

It is recorded because the GUI is relying on that without saying so, and
because "the other component checks it" is exactly the assumption that stops
being true. Setting `SymlinkFail` for a bundle produced by this application
costs one field and makes the GUI's behaviour correct on its own terms.

### 6.4 — Dev harnesses are correctly kept out of the shipped bundle

Checked because it is a common way for a debug page to end up inside a
product. `hack/copyfrontend`'s `isDevArtifact` excludes `*.demo.html`,
`gallery.html` and `*.md` from the recursive copy into `frontend/dist`, which
is what `main.go` embeds with `//go:embed all:frontend/dist`. The eight demo
harnesses and the design gallery are therefore not in the binary. None of them
contains an HTML sink in any case.

### 6.5 — A vendor URL containing `=` misbinds its `--digest` entry

**Severity: Low.**
**Confirmed by reading. The binary's behaviour on an unmatched `--digest` key
was NOT verified**, so the consequence below is reasoning, not observation.
**Update, third round: verified by execution, and the reasoning was right. The
core repository has since fixed the flag, but not for the command this
application runs. §6.5a has the transcript, why the first suggested remedy does
not work, what landed in core, and what the GUI does in the meantime.**

**Where:** `internal/cliadapter/iface.go`, `BuildArgv`:

```go
for _, u := range spec.URLs {
    if u.SHA256 != "" {
        argv = append(argv, "--digest", u.URL+"="+u.SHA256)
    }
}
```

`--digest` is a pflag `stringToString`, whose `Set` splits each pair on the
**first** `=`. A vendor URL carrying a query string — `https://vendor.example/
download?file=agent_2.1.0_amd64.deb` is an ordinary shape, not a contrived one
— therefore splits as key `https://vendor.example/download?file` and value
`agent_2.1.0_amd64.deb=<sha256>`.

The digest is the operator's own attestation that the file they are about to
ship is the file they checked. Misbinding it means either the build fails with
an error naming a URL the operator never typed, or — the worse branch — the
digest matches nothing, the download proceeds unattested, and the UI still
shows the digest as recorded. The tray already has a banner for the case where
a digest *cannot* be carried ("The expected SHA-256 was not recorded"); this
is the case where it is carried and silently lands on the wrong key.

**Recommendation:** percent-encode the `=` in the URL half before joining, or
ask the core repository for a `--digest URL SHA` two-argument form or a
repeatable `--digest-url`/`--digest-sha` pair. The first is a one-line change
here; the second is the honest fix and belongs in the core-repo backlog beside
the other `--json` gaps. Either way, a test should assert that a URL
containing `=` round-trips.

### 6.5a — Driven against the binary; fixed in core for `fetch`, not for `build`

**Status: the GUI half is done. The core half is done for `fetch` and is one
line away for `build`, which is the command this application runs — so the
defect is still live for Debark today and the GUI's refusal is still needed.**

**The observation.** Two builds against a local HTTP server returning a real
`.deb`, differing only in whether the vendor URL carries a query string, each
given the same deliberately wrong `--digest <url>=1111…1111`. `debark` built
from the engine at the third round's HEAD, run in `debark-shots:go126`.

| | URL | Outcome |
|---|---|---|
| A | `http://127.0.0.1:9201/agent_1.0_amd64.deb` | **exit 3.** `input.external` (warn): `sha256 mismatch: expected 1111…1111, got bcfb38a837b64a85cd70bfbef1d2129cb0e690b3b518368fb0eb3b75e3202768`. The digest bound; the wrong file was refused. |
| B | `http://127.0.0.1:9201/download?file=agent_1.0_amd64.deb` | **accepted.** `input.external`: `{"filename":"download","publisher_verification":"url-unverified","sha256":"bcfb38…2768","size":948}`. The same bytes, the same wrong digest, and the build carried on with the file unattested. |

That is the worse of the two branches §6.5 named, and it is the one that
happens. The operator supplied an attestation, the UI recorded it, and nothing
checked it.

Reproduce with `digest-probe2.sh`, which is not part of this repository. The
first attempt used an invented file rather than a real package and proved
nothing: `debark` rejects a non-`ar` body before it ever reaches the digest,
so both arms failed identically. The `.deb` has to be real for the difference
to appear.

**Why "percent-encode the `=`" does not work.** `internal/cli/cmd_build.go`'s
`buildInputs` reads `digests[val]` where `val` is the **exact positional URL**.
Encoding the key half makes it differ from that string, so the lookup misses in
precisely the same silent way — the same outcome as row B, reached by a
different route. The positional URL cannot be encoded to match either, because
that is the URL `debark` fetches. Quoting does not help: pflag reaches for
`encoding/csv` only when the pair holds two or more `=`, and csv does not
protect the `SplitN(pair, "=", 2)` that follows it.

So of §6.5's two suggestions, only the second is a fix.

**Two cases §6.5 did not reach.** §6.5 described the flag as splitting at the
first `=`, which is only what it does when the pair holds exactly one. With two
or more, pflag runs the whole value through `encoding/csv` **first**, and there
a comma is a field separator and a quote starts a quoted field:

| `--digest` value | What pflag binds |
|---|---|
| `…/a.deb=<sha>` | `…/a.deb` → `<sha>`. Correct. |
| `…/a.deb?ver=1=<sha>` | `…/a.deb?ver` → `1=<sha>`. Silent. |
| `…/a.deb?ids=1,2=<sha>` | **two** entries: `…/a.deb?ids` → `1`, and `2` → `<sha>`. Silent. |
| `…/a.deb?q="x"=<sha>` | nothing: `parse error on line 1, column 35: bare " in non-quoted-field`. Loud, and the only one that is. |

Three of the four are silent. The GUI's rule — refuse any URL containing `=`
when a digest is supplied — covers all four, because every one of them needs an
`=` inside the URL to happen at all.

**What the core repository did, and what it did not do.** debark commit
`92ebdc2` added `registerDigestFlag` (`internal/cli/digestflag.go`): a flag
type that splits the pair at the **last** `=` and validates the digest at parse
time. Splitting last is exact rather than a better guess — a SHA-256 is 64
hexadecimal characters and can never contain `=` — and the parse-time
validation is what makes it safe to rely on, because a genuinely malformed pair
becomes a loud error naming where the split landed rather than a quiet
misbinding. Its table covers all four pflag shapes above, including the
presigned URL whose signature ends in base64 padding
(`…&sig=abcd==<sha>`), which looks like it should break the rule and does not,
and it fails if the pflag defect is ever removed from under it.

**`fetch` adopted it. `build` did not.** `build`'s registration is in
`internal/cli/cmd_build.go`, which carried uncommitted work belonging to nobody
on that side, so the adoption was deliberately left as a documented one-line
change: `StringToStringVar(&digests, "digest", …)` becomes
`registerDigestFlag(flags, &digests)`, with `digests` and `buildInputs`
untouched.

`build` is the only command Debark runs. **So `build --digest` still misbinds a
URL with a query string, and everything above is still true of this product.**

**What the GUI does now.** `BuildSpec.Validate` refuses a spec that pairs a
SHA-256 with a URL containing `=`, naming the URL, saying what `build --digest`
actually does with it, and giving three ways forward: clear the expected digest
for that one URL and accept an unverified download — which is what would
silently have happened — use a URL without a query string, or download the file
and add it as a local `.deb`, where the digest is checked on disk. The hint also
names the observable that says a binary no longer has the limit: its
`build --help` describing `--digest` as split at the **last** `=`.

Refusing is not the comfortable answer and it is the right one. The three
outcomes available against a binary without the fix are a silent unattested
download, a build that fails naming a URL the operator never typed, and a
refusal before anything runs that says exactly what is wrong. Only the third
leaves the operator able to decide.

`BuildArgv` is deliberately left as a plain translation: it still emits the pair
it is given. Putting the refusal in two places would let them disagree, and a
translation that silently dropped the pair would hide, in the previewed command,
the very attestation the operator asked for. The argv it emits is *already*
correct for the fixed flag — a last-`=` split reads it the way the operator
meant — so nothing in the translation changes when `build` adopts
`registerDigestFlag`.

**What has to be true before the GUI's refusal can go, and it is two things.**

1. `build` registers `--digest` through `registerDigestFlag`. One line in
   `internal/cli/cmd_build.go`.
2. Every `debark` this application will run has that change. This is the part
   the GUI cannot assume, because it runs whatever binary it finds on `PATH` or
   in the operator's settings — the readiness screen exists precisely because
   that binary is not under this repository's control.

If the project can simply require a minimum `debark`, delete
`digestPairIsExpressible`, its call in `Validate` and the `=` rows in
`internal/cliadapter/digest_test.go`, and this section becomes history. If it
cannot, the detection is cheap and half-built already: `registerDigestFlag`
gives both commands one shared usage string containing `split at the LAST '='`,
and `Probe` already reads `build --help` to decide the `--sbom` capability, so
one more `Capabilities` field decides it from the same text. **Not** a version
comparison — this repository compares no version strings for anything, and rule
1 is why.

Tests: `internal/cliadapter/digest_test.go`.
`TestDigestMisbindsWhenTheURLCarriesAnEqualsSign` runs the argv this package
really builds through a transcription of pflag's `stringToStringValue.Set`
(v1.0.10, the version `debark`'s `go.mod` requires) and shows the digest
landing on the wrong key; the transcription is itself pinned against pflag's own
documented examples by `TestDigestTranscriptionMatchesPflagsOwnExamples`, so the
reproduction cannot pass because the copy is wrong. pflag is not imported: it is
not a dependency of this module and rule 6 says adding one is a decision to
report, not to make.

### 6.6 — The committed Windows installer requests admin and silently runs Microsoft's online WebView2 bootstrapper

**Severity: Low today (nothing ships from it yet); Medium the day a Windows
installer is released.**
**Confirmed by reading the committed scripts.**
**Status: open. For the Windows work and the dependency review.**

**Where:** `build/windows/installer/wails_tools.nsh:31` and `:175`,
`build/windows/installer/project.nsi:85`.

These are the stock Wails NSIS template, committed unchanged and, as far as
this review can tell, never read. Two lines matter:

```nsis
!define REQUEST_EXECUTION_LEVEL "admin"          # wails_tools.nsh:31
RequestExecutionLevel "${REQUEST_EXECUTION_LEVEL}"
…
ExecWait '"$pluginsdir\webview2bootstrapper\MicrosoftEdgeWebview2Setup.exe" /silent /install'
```

So the default Windows installer **elevates to administrator** and **silently
runs Microsoft's WebView2 bootstrapper**, which is the *online* installer: it
downloads the runtime from Microsoft **at install time, on the operator's
machine**.

> **Correction, third round.** This paragraph originally continued "meaning the
> Wails build fetches an executable over the network and embeds it in the
> installer". That is wrong and the packaging work found it: `wails`'
> `internal/webview2runtime/webview2installer.go` carries
> `//go:embed MicrosoftEdgeWebview2Setup.exe`, so the bootstrapper is already
> inside the `wails` CLI and is written out from there. There is no build-time
> fetch. The finding is unaffected — the network call is the one the
> bootstrapper makes at install time, which is the one that matters to an
> operator on a locked-down machine — but a security document with a wrong
> mechanism in it is worth less than one with a gap, so the sentence is
> corrected rather than quietly dropped.

Three reasons this belongs in a security review rather than in a packaging
ticket:

1. **It contradicts the product's own posture, on the platform where nobody
   noticed.** §5 of this document exists because a `.deb` that acquires root is
   indefensible for this audience; `internal/readiness/checks.go` says so in as
   many words. The Windows side already ships exactly that, as a template
   default that arrived with `wails init` rather than as a decision anyone
   made.
2. **It is a build-time network fetch of a third-party executable that ends up
   inside a distributed artefact.** That is a supply-chain fact and it belongs
   in `docs/dependency-review.md` with the rest of them, under "what its
   compromise would mean".
3. **It is silent at install time.** `/silent /install` means the operator is
   not told that installing this application caused a download from Microsoft.
   On the locked-down machines this tool is for, that is the kind of surprise
   that gets a tool banned.

This is **not** a rule-3 violation: `microsoft.com` is not a debark-operated
host, and the fetch is Microsoft's runtime rather than anything this project
serves. The rule-3 scanner now covers `build/` (it previously skipped the whole
directory, which is how this went unread — only `build/bin/` is output, and
the fix was to skip that by path instead), and `.nsi`/`.nsh`/`.plist`/`.ps1`/
`.desktop`/`.service`/`.policy`/`.rules` are now in the scanned extension list,
because an installer is the one artefact where a download URL does not look out
of place.

**Recommendation for the Windows work:** set `REQUEST_EXECUTION_LEVEL "user"`
and install
per-user; detect a missing WebView2 and *tell the operator* rather than
installing it for them — the readiness screen is already the right surface for
exactly that message, and it already tells the truth about Windows rather than
promising things. If a bundled bootstrapper is genuinely wanted, use
Microsoft's *offline* evergreen installer so nothing is downloaded at install
time, and record it in the dependency review with its digest.

### 6.7 — No Content-Security-Policy is served with the embedded assets

**Severity: informational (defence in depth; nothing is exploitable without
it).**
**Confirmed by reading `main.go`.**
**Status: FIXED in the third round by the low-severity security follow-up.
§6.7a records the policy that ships, what it blocks, what had to be relaxed,
and the evidence it was validated on the shipping engine rather than on
Chromium.**

`main.go:58` configures the asset server with the embedded `frontend/dist` and
nothing else:

```go
AssetServer: &assetserver.Options{
    Assets: assets,
},
```

No `Middleware`, so no response headers, so no CSP. Everything else about the
window is set carefully — the GPU policy is restated to defeat a Wails default
that would silently re-enable acceleration, the title-bar theme is spelled out
in six colours — which makes the absence of a header worth naming rather than
assuming it was considered.

Nothing in §2 is exploitable, so this changes nothing today. What it changes
is the cost of being wrong tomorrow. Every property in §2 and in §4.6's
frontend scan is a property of *the source as written*: one `innerHTML`, no
`fetch`, no remote subresource. A CSP makes them properties of *the runtime*,
enforced by the webview against code that has not been written yet — and the
whole argument for `internal/audit` is that the dangerous commit is the one
nobody has made.

A policy this application could adopt as-is, because it already satisfies
every clause:

```
default-src 'none';
script-src 'self' 'sha256-<the theme bootstrap>';
style-src 'self';
img-src 'self' data:;
font-src 'self';
connect-src 'none';
form-action 'none';
frame-ancestors 'none';
base-uri 'none';
object-src 'none'
```

`connect-src 'none'` is the interesting line: it makes rule 3's frontend half
enforced rather than merely true, so a `fetch()` added by a future commit fails
in the browser as well as in `TestFrontendHasNoNetworkPrimitive`.

One wrinkle, and it is small: `frontend/index.html` carries a deliberate inline
`<script>` in `<head>` that applies the stored theme before first paint, and
its comment explains why it cannot be deferred. `script-src 'self'` would block
it, so the policy needs that script's SHA-256 — which is stable, since the
script is fixed — rather than `'unsafe-inline'`, which would give back most of
what the header buys.

**Recommendation:** add an `assetserver.Middleware` that sets the header, and
pin the theme script's hash with a test that recomputes it from
`frontend/index.html`, so an edit to the script fails the build rather than
silently disabling the theme.

### 6.7a — What shipped, what it blocks, and what was relaxed

**Status: done.** `main.go` installs an `assetserver.Middleware` that sets
`Content-Security-Policy` on every response the asset server produces. The
policy is built at startup by `assetContentSecurityPolicy`, which reads the
embedded `frontend/dist/index.html` and hashes each inline `<script>` in it.

**The exact header, as served on 2026-09-06 from the tree at that time:**

```
default-src 'none'; script-src 'self' 'sha256-sU0uCIacH9/Cnsim70ojdge3uIyQld5kX296oxab10c='; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'none'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'
```

The `sha256-` value is the theme bootstrap in `frontend/index.html`. It is
**derived, not pinned**, so the value above is the record of one build rather
than a constant anyone has to maintain — see *the one deliberate departure*
below.

**What it blocks.** Everything not named, because `default-src 'none'` is the
floor: there is no `media-src`, `worker-src`, `manifest-src` or `frame-src`
clause, so no `<audio>`, `<video>`, worker, service worker, manifest or frame
can load at all. Beyond that:

| Directive | What it stops |
|---|---|
| `script-src 'self' 'sha256-…'` | Every inline script but the one hashed theme bootstrap; every `javascript:` URL; every inline `on*` handler; every script from another origin; `eval` and `new Function`. |
| `connect-src 'none'` | `fetch`, `XMLHttpRequest`, `WebSocket`, `EventSource` and `navigator.sendBeacon` — all of them, to every destination. This is the clause worth having: rule 3's frontend half stops being a property of the source that `internal/audit` reads and becomes a property of the runtime. It does not touch the Wails bridge, which is `webkit.messageHandlers.external.postMessage` and not a network fetch — confirmed by reading the runtime Wails serves, which contains no `fetch`, no `XMLHttpRequest`, no `WebSocket` and no `eval`. |
| `img-src 'self' data:` and `font-src 'self'` | Every off-origin image and font, including the CSS `url()` fetch that is how injected CSS exfiltrates. |
| `form-action 'none'` | Submitting anywhere. There is no form in the app. |
| `base-uri 'none'` | A `<base>` element re-pointing every relative URL in the document — the standard way an injected tag turns same-origin script paths into someone else's. |
| `object-src 'none'` | `<object>`, `<embed>`, `<applet>`. |
| `frame-ancestors 'none'` | Anything framing this document. |

There is deliberately **no `report-uri` and no `report-to`**. A violation
report is a network call and rule 4 forbids telemetry of every kind; a test
asserts their absence rather than a comment claiming it.

**What was relaxed, and why: `style-src 'self' 'unsafe-inline'`.**

§6.7's ready policy says `style-src 'self'`. That policy breaks the package
picker. Three screens build their layout stylesheet at runtime —
`frontend/src/screens/picker-list.js:1511`, `picker-search.js:194` and
`picker-tray.js:127` each call `document.createElement('style')`, set
`textContent` and append to `<head>` — and a `<style>` element's text is
checked against `style-src` whether it was parsed from markup or created by
script. Under `'self'` alone all three are dropped and the picker loses its
grid, its search row and its tray.

The relaxation is real, and its blast radius is small for a reason that is
worth stating rather than assuming: CSS attacks work by *fetching* something —
a background image, a font, an `@import` — and `default-src 'none'` with
`img-src 'self'`, `font-src 'self'` and `connect-src 'none'` leaves injected
CSS nowhere to send anything. What `'unsafe-inline'` costs here is defacement,
not exfiltration, and it does not weaken `script-src`, which is where the value
of this header is.

**The change that removes it, for whoever owns `frontend/src` next.** Move the
three `SCREEN_CSS`/`LAYOUT_CSS` constants into `frontend/src/design/` as
ordinary `.css` files and load them with a `<link>`, the way `main.js:48`'s
`addStylesheet` already loads `tokens.css` and `components.css`. A `<link>` is
a same-origin subresource, so `style-src 'self'` covers it with nothing
relaxed. That is three files added, three string constants deleted and three
`installLayoutCSS`/`injectScreenCSS` functions deleted; no other directive
changes. It was **not** done here because `frontend/src/**` belonged to
the accessibility follow-up while this work was live.

**The one deliberate departure from the recommendation above.** §6.7 asked for
a pinned hash plus a test that recomputes it. What ships derives the hash at
startup instead, and the reason is the failure mode. A pinned hash that falls
out of step with the script does not fail loudly: it blocks the theme
bootstrap, and the only symptom is the app opening in the wrong theme with
nothing on screen to say why. Nobody would connect the two. A derived hash
cannot fall out of step.

The review's actual requirement — that nobody adds an inline script without a
person reading it — is met by `TestIndexHTMLHasExactlyOneInlineScript`, which
fails when `index.html` carries anything but the single theme bootstrap, and by
`TestContentSecurityPolicyKeepsTheRulesTheProjectCannotRelax`, which fails if
`script-src` ever gains `'unsafe-inline'`, `'unsafe-eval'`, `'unsafe-hashes'`
or `*`, or if any directive gains a reporting endpoint.

**Deriving the hash is only sound if the hashed bytes are the bytes the engine
sees, and they are not obviously the same.** Wails does not serve `index.html`
as embedded: `pkg/assetserver`'s `processIndexHTML` re-parses it with
`golang.org/x/net/html` to inject `/wails/runtime.js` and `/wails/ipc.js`, then
re-serialises the whole document. If that round trip changed one byte inside
the inline script the shipped hash would be wrong and the script would be
silently blocked. `TestServedIndexHTMLMatchesItsOwnHash` drives the **real**
`assetserver.NewAssetServer` with the **real** middleware, checks the served
document really did gain the injected runtime tags, and then compares the hash
in the response header against a hash of the inline script in the response
body. It passes: the round trip is byte-exact.

**Validated on WebKit2GTK, by photographing the app.**

This is the half that cannot be done by reading, and Chromium could not have
done it either: WebKit's CSP implementation and its handling of the `wails://`
custom URI scheme are their own thing. The finding that matters most to anyone
repeating this: **a CSP violation on this stack is completely silent.**
WebKitGTK does not write console messages to the web process's stderr unless
`webkit_settings_set_enable_write_console_messages_to_stdout` is set, and Wails
does not set it. Across every run below the app's stderr held exactly two
lines — an AT-SPI bus warning and a JSC signal notice — whether the policy was
correct or fatal. A clean stderr here is not evidence of anything. **The only
instrument that works is a screenshot.**

Image `debark-shots:go126` — Ubuntu 24.04.4, WebKit2GTK 4.1 = 2.52.6, GTK3,
Xvfb at 1400x900, `WEBKIT_DISABLE_COMPOSITING_MODE=1`, built with
`-tags "desktop,production,webkit2_41"`. A `debark` built from the sibling
repository was put on `PATH`, so the gated stepper could be walked and a
catalogue really built.

*Every screen, under the shipped policy, in one running process:*

| Screen | Result |
|---|---|
| Readiness | Renders identically to the pre-CSP reference shot, apart from the elapsed-time figure. |
| Target | Base picker, radio group, warning banner, definition table and "Use this base" all present. |
| Gated stepper | Clicking a locked step raises its explanatory popover ("Choose a target first"), so the popover path is exercised too. |
| Packages, empty | The "No catalogue for this target yet" state and its action. |
| Packages, live | A catalogue built for real against `deb.debian.org` (Debian 12 minimal; 265 matches for `browser`), the virtualiser scrolling, a row selected, the tray showing its chip and size estimate. **This is the picture that proves the `style-src` relaxation is both needed and sufficient**: all three runtime `<style>` elements are in force in it. |
| Build | The whole form — output folder, signing, build options. |
| Export | The drive table, four rows, each with its own refusal reason. |
| Light / Dark / System | All three themes applied at runtime and rendered correctly. |

*And the controls, because "the app still works" is not evidence that a header
is being enforced.* Three further binaries were built from a patched **copy**
of the tree, in `/tmp` inside the container, never in the repository:

| Control | Policy | Result |
|---|---|---|
| `noscript` | `script-src 'none'` | **Blank window.** The stylesheets still load and paint the page background; not one module runs. This is the proof that the header reaches WebKit through the `wails://` scheme handler and is enforced there. |
| `probe-allowed` | the shipped policy, with a second inline script added to `index.html` that paints the window magenta from `DOMContentLoaded` | Window is `srgb(255,0,255)`. The inline script ran — so an inline script allowed by a hash **this code computed** executes on WebKit 2.52.6. |
| `probe-blocked` | the same document, `script-src 'self'`, hashes withheld | Window shows the normal app, `srgb(242,244,246)` at the same pixel. The inline script was blocked. |

The `probe-allowed`/`probe-blocked` pair is what makes the theme bootstrap safe
to rely on: the two builds differ only in whether the hash is in the header,
and that difference decides whether an inline script runs.

Scripts and screenshots — `ctl-build.sh`, `drive3.sh`, `drive4.sh`,
`drive5.sh`, `drive9.sh` and `out/shots/` — are not part of this repository.

**What is still not covered.** The Windows/WebView2 side was not run: this
review had no Windows webview harness and Linux is the shipping target. The same
middleware sets the same header there and WebView2 is Chromium-based, so the
hash and the `style-src` relaxation are if anything more likely to work — but
that is an expectation, not a measurement, and it is written here as one.

**Re-run after the frontend was rewritten under it.** The walk above was taken
against the tree as it was when the policy landed. The accessibility follow-up
then reported done, having landed four commits across `frontend/src/**` —
including a new `shell/disclosure.js` that all fourteen disclosure drawers now
route through —
so the application was rebuilt from HEAD and walked again. **A policy proved
against a tree two commits old is not proved.**

Same result. Every screen renders, in light, dark and system, and the app's
stderr held the same two unrelated lines and nothing else. Four surfaces the
first walk had not reached were covered this time and all are fine:

- **A disclosure drawer**, opened and closed through the new shared module.
- **The toast host**, raised by pressing a gated primary action.
- **The command-preview panel** on the build screen — §6.1's own render site —
  showing a real `debark build …` line under its "Copy the command" button.
- **A gated step's popover**, raised by clicking a locked stepper item.
- **The build screen's progress pane**, the view that replaces the form once a
  build starts, driven by actually starting a build.

The frontend was re-scanned for CSP-relevant sinks at the same commit, because
the relaxation is only justified for as long as the thing that forces it is the
only thing there. Unchanged: the same three `document.createElement('style')`
sites in `picker-list.js`, `picker-search.js` and `picker-tray.js`; no
`setAttribute('style'|'href'|'src')` anywhere; no `javascript:` URL, no inline
`on*` handler, no `eval`, no `new Function`; no `fetch`, `XMLHttpRequest`,
`WebSocket`, `EventSource`, `sendBeacon` or worker; and `index.html` still
carrying exactly one inline script, so the `sha256-` value recorded above is
still the one that ships.

---

## 7. Reported to other packages

Collected so nothing here is lost in a section a reader will not open.

**To whoever owns `internal/catalog`:**
- §3.1, scheme allow-list on archive URIs — the most valuable item in this
  review.
- §3.2, decompressed-size cap in `packages.go`, mirroring `dep11.go`.
- §3.3, `CheckRedirect` on both clients.
- §4.4, reject a negative `entry_count`/`bin_size` in `ReadCacheMeta`, and
  reconcile the `BinSize != 0` exemption in `cacheLoadIndex` with
  `CacheReadyDir`, which has none.
- Confirmed fixed and now covered by named cases:
  `internCount > cacheMaxInterned` → `ErrCacheCorrupt`.

**To whoever owns `frontend/`:**
- §2.5, the unclamped percentage in `picker-list.js:1484`.
- §2.5, `ICON` as a plain object literal; `Object.create(null)` or a `Map`
  would make `svg()` safe by construction rather than by call-site discipline.
- §6.1, redact userinfo in the copied command preview.
- Nothing in `frontend/` needs a security fix. The escaping holds.

**To integration:**
- §4.1a, two load-sensitive tests in `internal/catalog` —
  `TestCacheReaderRacingWriter` (fails at `-count=3` under load with "load
  during a rebuild: catalog: not built for this target") and `dep11_test.go`'s
  10-second parse budget (measured 12.0 s on a busy machine). A performance
  budget asserted inside a unit test measures the runner.
- §4.1a, bound `-parallel` for any fuzzing that runs in CI, or a green job will
  occasionally report a failure nobody can reproduce.
- §6.7, a Content-Security-Policy on the asset server. Cheap, and it converts
  every source-level property in §2 into a runtime-enforced one.
- §6.5, the `--digest` misbinding on a URL containing `=`. This is the one
  finding here that can silently weaken a check the operator believes is in
  force, and it is cheap to fix.
- `internal/app` never sets `readiness.Options.ArchiveHost`, so the
  `archive-network` check is never registered and never runs. Safe direction,
  but it is a check the readiness screen was designed around and it is
  currently dead.
- §6.2, `RevealPath`'s URL construction.
- §6.3, set `SymlinkFail` on export.

**To packaging:** all of §5.

**To the Windows installer work:** §6.6. The committed NSIS template elevates
to administrator and silently runs Microsoft's online WebView2 bootstrapper. Nothing ships from it
yet, which is why it is Low; it stops being Low on the day something does.

**To the dependency review:** the link closure is 327 packages from
`go list -deps .`; the npm section is one line and it is the good kind.

---

## 8. What this review did not cover

Stated plainly, because a reviewer reading a green record deserves to know its
edges.

- **The `.deb` does not exist**, so §5 is a checklist, not a result. Rows 19
  and 20 in particular have never been executed.
- **No end-to-end run against a real `debark` binary.** The rendering pass
  drove the real screens against a hostile bridge, not against real `debark`
  output. A real failing build's stderr is the one untrusted-text source this
  review saw only as a fixture. The end-to-end run is where that gets closed.
- ~~**§3.2 was not demonstrated.**~~ **Closed — see §9.2.** A bomb was built
  and its expansion ratio measured at 293:1, so the 256 MB this path accepted
  really was ~75 GB of text. The estimate the finding reasoned from (~1000:1)
  was high for realistic stanzas; the conclusion was right either way.
- **The Wails bridge itself was not audited.** Everything here treats
  `window.go.app.App` as a trusted transport. Wails' IPC, its asset server and
  its `BrowserOpenURL` are third-party code; what they do belongs in the
  dependency review, not here.
- ~~**No fuzzing of the deb822 sources parser or the DEP-11 YAML parser.**~~
  **Closed — see §9.5.** Five targets, 113,571,594 executions over 50 minutes.
  It was the right thing to name as the largest gap: the sources targets found
  five distinct defects, four of them the same mistake in different clothes.
  One surface named here is still open — `ParsePackagesIndex` has no target of
  its own — and §9.5 says so.
- **The readiness pack's subprocess execution was read, not exercised.**
  `ExecRunner` builds an argv vector and never a shell string, which is the
  property that matters, but no adversarial test drives it.
- **`internal/audit` reads the source tree, not the shipped binary.** A build
  injecting a host through `-ldflags -X` would pass every check in §4.6.

---

## 9. Remediation — what the integration work fixed, and what it found doing it

Written by whoever acted on this review, appended rather than woven in,
so the review stays a record of what was true when it was taken and this stays
a record of what was done about it. Each subsection names the commit, says what
was verified by running rather than by reading, and — where the fix turned out
to be more interesting than the finding — what it exposed.

Four findings are closed: **§3.1**, **§3.2**, **§3.3** and **§4.4**. The largest
gap §8 named against this review — no fuzzing of the deb822 or DEP-11 parsers —
is closed in §9.5, which is the longest subsection here for a reason: it found
a defect in §3.1's own fix within fifteen seconds, and four more after that.

### 9.1 §3.1 — the URI scheme allow-list (`f072e1b`)

`Target.IndexRefs` now admits `http` and `https` and nothing else. Everything
refused becomes a `SourceProblem` of a new kind, `unsupported-scheme`, which
`Deliberate()` reports true for: an install-media or local-mirror source is a
documented scope limit, not a fault in the operator's file, and a UI should show
it the way it shows the deb-src stanzas it always skips.

**Three decisions in the fix are worth stating, because each could have gone the
other way.**

**`http` stays on the list.** Ubuntu still publishes plain-`http` archive URIs
and a target naming one is ordinary rather than suspicious. Refusing it would
empty the catalogue for a large and entirely legitimate population, to close
nothing: the finding is about the *set of protocols* an untrusted file can steer
this process into, and that set was never anyone's choice. The host set was
never bounded and still is not.

**The filter is at the point a ref is built, not at the point a document is
parsed.** `IndexRef.URL` is string concatenation and its result goes straight
into `http.NewRequestWithContext`, so the last place before a URL exists is the
only place that can promise a `Target` assembled by hand — a test, a cache
round-trip, a caller written next year — cannot reach the network with a scheme
nobody allowed. The *sentence* an operator reads is produced separately, once
per resolution, by a new `IndexRefsWithProblems` whose second return the three
`Resolve` functions append to what they already returned. No signature changed
and no new plumbing exists; the existing rendering path carries it.

**The scheme is parsed by hand, not by `net/url.Parse`.** apt URIs are not all
valid URLs — `cdrom:[Ubuntu 24.04]/` has an unescaped space and brackets — so a
parse failure would be indistinguishable from an unsupported scheme, which is
two different sentences for the operator. Everything before the first colon,
validated against RFC 3986's scheme grammar, is what apt's own method dispatch
uses and all this decision needs.

Standing tests replace the review's one-off experiment: the same seven URIs it
drove by hand, plus the shapes that are not schemes at all, plus a Windows path
(whose naive scheme `c` is safe here only because it is not on the list, and is
recorded so a later change cannot make it `http`).

### 9.2 §3.2 — the decompressed-size cap, now demonstrated (`73e82da`)

`PackagesLoader.MaxDecompressedBytes` exists, is 512 MiB, and is deliberately
the same number and the same field name `dep11.go` has always used. A test fails
if the two ever diverge again, since the asymmetry is what made this a defect
rather than a judgement call.

**§8 said this was "not demonstrated". It is now.**
`TestPackagesGzipBombIsRefused` builds a bomb out of the same stanzas a real
index carries and *measures* the ratio rather than estimating it:

```
229,050 compressed bytes inflate to 67,108,864 — 293:1
```

The test asserts the ratio too, so the argument for the cap stays true rather
than becoming folklore. At 293:1, the 256 MB this path already accepted was
about **75 GB** of text, on a machine whose whole idle-memory budget is 400 MB.
(The review estimated ~1000:1 from pure repetition; 293:1 is what realistic
stanzas achieve, and it is the honest number to quote.)

**A second cap was added that the finding did not ask for, and it is the one
that closes the hole properly.** A byte cap alone is not enough: a minimal
stanza is thirteen bytes, so 512 MiB of *accepted* text is about 41 million
`Entry` values the caller retains — several gigabytes of live heap, entirely
inside a limit that was only ever about bytes on the wire. `MaxEntries` is
1,048,576 per index, more than an order of magnitude above any real one, and a
test measures a real fixture against both caps so that a limit a legitimate
archive brushes against would be caught here rather than in the field.

Both caps are applied in `packagesParseBody` rather than in the exported
`ParsePackagesIndex`, which takes a reader: a caller who hands it a stream has
already decided how big that stream is. An uncompressed body is deliberately
left alone, because `packagesGet` already bounded it on the way in and capping
it twice would report a limit that did not apply.

### 9.3 §3.3 — `CheckRedirect`, with one of the three redirects still allowed (`73e82da`)

Both clients now share one policy. **Two of the three redirects this finding
named are refused and one is not**, and the reasoning is in the code because the
allowance is the part someone will otherwise "fix" later.

**Refused:** a redirect to any scheme not on §9.1's allow-list — the same rule,
applied a second time to a URI a *server* chose rather than one a snapshot
named, so a redirect cannot reintroduce the protocol set the allow-list just
removed. And a downgrade from `https` to `http`, which is the only redirect that
can take a fetch the operator's own sources asked to protect and make it
unprotected. The chain is also bounded at five rather than Go's ten.

**Allowed, deliberately: a redirect to another host.** A mirror redirector that
sends a client to a nearby mirror is ordinary apt infrastructure — it is what
`mirrors.ubuntu.com` and the httpredir generation of Debian services exist to
do. Refusing it would break legitimate targets to close nothing, because §3.1
already establishes that the host set is whatever the snapshot names: a snapshot
that wanted to reach another host could simply name it. This is a place where
the cheap check and the correct one differ, and the tests pin the allowance as
deliberately as they pin the refusals.

**One thing the fix ran into is worth recording**, because it is §4.6 working
exactly as designed. The policy was first written beside the scheme allow-list
in `sources.go`, and `internal/audit`'s
`TestOnlyInventoriedFilesTouchTheNetwork` failed: `sources.go` had acquired a
`net/http` import and is not on the seven-file inventory. That test says in its
own failure message that adding an entry is a design decision rather than a test
fix, and it was right — the parser that reads `/etc/apt/sources.list` has no
business holding an `http.Request`. The policy moved into `packages.go`, which
is already on the inventory for the reason it exists, and the inventory is
unchanged at seven entries.

### 9.4 §4.4 — negative counts, and the `bin_size` disagreement under it (`bb05231`)

`cacheReadMeta` refuses a negative `entry_count` or `bin_size` as
`ErrCacheCorrupt`, and returns a zero `CacheMeta` alongside the error so a
careless caller cannot use a half-filled struct. The severity assessment in §4.4
stands unchanged — nothing downstream was harmed — and the fix is justified by
its own argument: the next caller may be the one that writes
`make([]X, meta.EntryCount)`.

`FuzzSecCacheMeta`'s comment explaining why it deliberately did *not* assert
this is replaced by the assertion it describes. That comment was correct when it
was written, for a reason worth preserving: asserting a property the code did
not have would have left a red test in a package that package did not own.

**The "related, noticed while tracing it" note is fixed too**, and it was the
more interesting half. `cacheLoadIndex` guarded its size cross-check with
`meta.BinSize != 0 &&`, treating zero as "unknown", while `CacheReadyDir`
compared outright — two paths disagreeing about what zero means, with only one
reachable without going through `Ready`. Resolved in `CacheReadyDir`'s favour:
zero means zero. A cache this build wrote always records a real size, so
"unknown" can only come from a file `CacheWrite` did not write, which is
precisely what a cross-check exists to catch. Nothing older breaks —
`cacheReadMeta` refuses a foreign `FormatVersion` before that line is reached —
and an honestly empty catalogue still matches, because the image is then zero
bytes as well.

### 9.5 §8's largest gap — the sources and DEP-11 parsers are fuzzed now

§8 named this against the review itself: "No fuzzing of the deb822 sources
parser or the DEP-11 YAML parser. Both read attacker-adjacent bytes ... **This
is the largest untested surface left**, and it is the first thing a second
review should take."

Five targets now cover both. Every budget below was spent on the tree as it
stands, with **6 of this machine's 24 cores** — §4.1a found that all-workers
runs make workers die under load and write inputs that do not reproduce, so a
quarter of the cores is the setting that produced trustworthy results there
and it is the setting used here.

| Target | Executions | Wall | Result | New coverage (corpus) |
|---|---|---|---|---|
| `FuzzSecSourcesOneLine` | 43,269,573 | 10 m 01 s | pass | 590 (938) |
| `FuzzSecSourcesDeb822` | 34,105,721 | 10 m 01 s | pass | 513 (806) |
| `FuzzSecSourcesFile` | 27,768,511 | 10 m 01 s | pass | 476 (929) |
| `FuzzSecDEP11Decode` | 4,464,091 | 10 m 01 s | pass | 692 (712) |
| `FuzzSecDEP11Body` | 3,963,698 | 10 m 01 s | pass | 409 (449) |
| **total** | **113,571,594** | **50 m 05 s** | | |

The DEP-11 targets are an order of magnitude slower per execution because each
one runs a YAML decode under a deadline; the sources targets are string work.
Both DEP-11 targets were clean on their first full run and have not needed a
second.

**The sources targets were not.** They found five distinct defects across four
rounds, and every one is fixed above or below. This is the part worth reading,
because the pattern in them is more useful than any individual bug.

**1. An `http`/`https` URI with no host** (15 seconds into the first run; all
three targets, independently). `URIs:https:` passes any scheme test — the
scheme really is `https` — and `IndexRef.URL` concatenated it into
`https:/dists/0/0/binary-amd64/Packages.gz`. Fixed by requiring an authority.
Commit `3b0133b`.

**2. `SourceProblem.Reason` and `.File` were not UTF-8-sanitised, and `.Text`
was.** An `Architectures:` field holding invalid bytes reached a rendered
sentence verbatim. `encoding/json` substitutes U+FFFD silently rather than
failing, so it crossed the bridge and nothing downstream would ever have
complained. The same property is enforced for the catalogue's own strings by
`cacheClip`; two halves of one package disagreeing about it is what makes it a
defect. Commit `fd2ad2a`.

**3. A mixed-case scheme — NOT a defect.** `httP://0/...` was reported and was
not a bug: schemes are case-insensitive, `url.Parse` lower-cases, and
`http.NewRequest` sends it correctly (measured). The *test* was wrong, having
checked the rule with `strings.HasPrefix` instead of through the code that
implements it. Recorded here rather than quietly dropped, because a fuzz
finding that turns out to be a wrong assertion is worth as much on the record
as one that is not. Commit `2d17511`.

**4. A suite or component that cannot form a URL.** A component named `%`
builds `.../dists/0/%/binary-amd64/Packages.gz`, which `url.Parse` refuses with
`invalid URL escape "%/b"` — so the failure arrived from inside the fetch as a
network-shaped error for a malformed source. That is §3.1's own complaint about
`cdrom:` and `file:`, reaching the same place by a different route. Commit
`cb562d1`.

**5. `http://@`, and a vertical tab accepted as an archive URI.** The
hand-written authority check from finding 1 was itself beaten: `@` is a
non-empty authority with an empty host. Separately, the one-line parser splits
on space and tab — which is what apt does — so `deb \v 0 0` left a vertical tab
standing as the URI, which then reached `IndexRefs` and was dropped there in
silence. Commit `a9fd419`.

**The pattern, which is the finding under the findings.** Four of the five are
the same mistake in different clothes: *a rule about URLs was checked with a
copy of the rule instead of with the parser that would enforce it.* Each fix
was correct and each was beaten by the next input. The version that survived
43 million executions does the scheme by hand — deliberately, because
`url.Parse` cannot name the scheme of `cdrom:[Ubuntu 24.04]/` at all, and "this
source is install media" is the sentence that URI needs — and then hands
everything else to `net/url`, which is what `http.NewRequestWithContext` parses
with. The two cannot disagree because they are the same code.

The same lesson applies to the targets: the property that found four of these
is asserted *through* `Target.IndexRefsWithProblems`, not against a
re-statement of what it should do.

**Every finding was reproduced deterministically before being reported**, per
§4.1a's warning about workers dying under load. Not by re-running the corpus
entry — that only proves the fuzzer is consistent — but by writing a named
table test that reaches the same state without the fuzzer at all. All fifteen
minimised inputs are committed under
`internal/catalog/testdata/fuzz/`, so they run as seeds on every `go test`,
including the two from finding 3 that were never failures.

**What is still not fuzzed**, so the next reviewer inherits an honest edge:
`ParsePackagesIndex` has no target of its own. It is exercised indirectly —
`packagesParseBody` is covered by the gzip-bomb and entry-cap tests in §9.2 —
but the deb822 stanza scanner that produces 70,000 `Entry` values from
attacker-adjacent bytes deserves the same treatment the sources parser has just
had, and on this evidence it would find something.

---

*Finished 2026-09-06, 19:35 PDT, against `46ec3ca` plus the second-round tree as
it stood at `f774766`. Other packages were committing into the tree throughout —
six commits during the review, three screens still modified at the close.
Every finding above was re-read at the closing timestamp, and the rendering
pass was re-run against the changed screens.*

## Desktop simplification hostile UI audit, 2026-09-07

This is a scoped rendering audit of the simplified desktop workflow. It does
not repeat the earlier engine, cryptography, packaging or parser-fuzz review.
The original §2 run remains historical. Its blanket dialog opening and button
clicking did not assert that the current workflow reached the intended
bindings, so it could not certify the redesigned screens.

Harness commit `1a67d6f` replaces that driver with visible sequential
journeys. It preserves legal fixture lifecycle flags, immutable identities,
revisions, paging and echoed search queries, then projects hostile strings
through the generated schema. Every supplied fixture result is validated
before hostile projection. Unknown methods fail rather than receiving a
successful fallback. The generator discovers all 53 method declarations and
55 classes in the recorded schema, including nested generic returns that the
old expression skipped.

The driver opens disclosure summaries and actual dialog actions, respects
modal scope, and refuses missing, ambiguous, hidden or disabled controls.
It scans body-mounted Add/tray dialogs as well as the app root. A required
binding that was not reached makes the run fail. The same is true of absent
visible verbatim payloads, missing HTTPS control linking, JavaScript errors,
any injection signal, incomplete execution or a source change during the run.
A missing-browser negative check returned exit 1 and cleaned up its temporary
profile. This prevents an empty or blocked walk from being called a pass.

### What ran

The three JSON reports and accompanying local transcripts were read after the
runs. Each completed with `done: true`, an empty `problems` array, zero
`totalProblems`, and a successful runner exit. All ten named journeys passed
and all 32 reached binding names were the same in all modes.

| Mode and durable report | Start time, UTC | Projected field paths | Visible positive evidence | Result |
|---|---|---:|---|---|
| realistic | 2026-09-07 19:12:01.447 | 216 | Full description, label, path, hostile URL and stderr payloads as visible text; no hostile link | Exit 0, zero problems |
| poison | 2026-09-07 19:12:03.887 | 228 | Same visible hostile strings; invalid enum/error events followed by authoritative recovery and a reached chooser | Exit 0, zero problems |
| control | 2026-09-07 19:12:06.275 | 216 | Description, label, path and stderr text; `CONTROL_URL` text and its visible HTTPS anchor | Exit 0, zero problems |

Environment: Windows `win32/x64`, Node **24.13.0**, headless Chrome
**152.0.0.0**. The actual reported viewport was **1014×800 CSS pixels**, device
pixel ratio **1**. Chrome was launched with `--window-size=1040,900`; that
argument is not a measurement of the viewport. No native Wails binary, native
file chooser or assistive technology was involved in these runs.

All three reports carry the same full source-manifest SHA-256:

```text
bddcd3538d9bfe45f522034e9800f151b875088b4a25e83f078cdef3a44000c6
```

The manifest includes the actual frontend source/CSS, harness, shared fixture
and generated model/declaration files read for the run. The schema hashes are:

```text
models.ts: fedfb41626f90f035392345097b3ded1d9fa9eb71abf542cfc3f93077ea21d96
App.d.ts:  c57db43c0163e921ef0b0a9198cf2404cac2638ef400afa8312a931726eaba7d
```

The original reports were written to a working directory kept outside this
repository, which is where the raw run output stays; the linked copies preserve
these report hashes:

```text
realistic.json: 1995bd52a9aca12e8627c5cdf9ec4575d934ab77029050cd608b8b03bd61fe25
poison.json:    93c1d44c85017e9a3379efaaa49c6c34baad5f54f45f557d6182d19034af6166
control.json:   4b635c62a9e193e51910ef44338b247e0ad716221add17a9df43a3d918d838bb
```

### Reached paths and safeguards

The walk reached System check through the main menu; snapshot choose/inspect
and Target Continue; catalogue search; Enter → `GetPackage` → Details-close
with list focus restoration; selected-item Details; paste preview and add;
URL validation with the supplied SHA-256 retained; and one two-file chooser
result passed to `AddLocalDebs` exactly once. In Bundle it reached output and
signing-key choosers, `PreviewCommand`, `StartBuild`, status recovery and log
reading. Copy reached an explicitly selected destination, `InspectDestination`
and `PlanExport`; the main menu also reached existing-bundle source selection.
A synthetic job completion followed a user-started fixture build. It was not
an actual CLI build.

The reached methods were:

```text
AddLocalDebs AddPackageList AddURLs AppInfo BuildLog BuildStatus
CatalogStatus Categories ChooseDirectory ChooseLocalDebs ChooseSigningKey
ChooseSnapshotFile CurrentTarget ExportStatus GetPackage InspectDestination
InspectSnapshot LifecycleStatus ListBases ListVolumes ParsePackageList
PlanExport PreviewCommand Readiness SearchPackages SelectTarget Selection
SelectionKeys SelectionPage StartBuild StartCatalogBuild SupportedArchitectures
```

Four negative signals remained clear: no `window.__xss` executions, foreign
markup, event-handler/dangerous URL/style attributes, or off-origin resource
and instrumented JavaScript requests. The positive checks require whole
payload strings in **visible text nodes**; report text and closed content
cannot satisfy them. Control mode produced a visible
`https://vendor.example/pool/CONTROL_URL.deb` anchor from the same warning
field that receives a rejected `javascript:` URL in realistic mode.

Poison mode is now an explicit, bounded invalid-enum/error event phase with
recovery. It does not poison every discriminator at startup and then mistake
unreachable controls for safe rendering. Structural fields such as an echoed
search query remain unchanged: altering that echo makes the virtualizer
correctly discard the response, which tests stale-result rejection rather
than rendering safety. The exact projected field paths are in each report;
216 or 228 paths are not a claim to cover every string in all bridge classes.

### Limits and reproduction

This confirms those reachable renderer paths on the recorded Chrome snapshot.
It does not establish native Linux/WebKitGTK behavior, the production CSP,
native keyboard input, private-key handling inside Go, filesystem cleanup,
actual download/copy verification, or trusted offline installation. The
fixture's native-chooser-named journey observes a fabricated chooser return;
it never opens a native chooser. Twenty-one of the 53 generated methods were
not reached. Native Quit/close races, Undo, copy cancellation and verification
have separate tests and must keep their own evidence labels.

The scoped Go audit passed after the harness change (`go test -tags
webkit2_41 -count=1 ./internal/audit/...`, 0.820s). That source audit complements
this rendering walk; it cannot replace runtime coverage, and neither result
certifies every future edit. Full source hashes make later changes explicit.

Run `node internal/audit/xssharness/run.mjs realistic`, then `poison` and
`control`. Each runner invocation regenerates its schema and returns nonzero
on a failed/incomplete audit. Set `DEBARK_XSS_REPORT` to save a structured
JSON artifact in an existing directory. Exact operating details, current
coverage assertions and failure signals are in the
[harness README](../internal/audit/xssharness/README.md).

### Integration follow-up, 2026-09-07 20:41 UTC

The same strict driver was rerun after copy-status commit `66f97ff` and
list ancestry/cursor commit `c48bd61`, including the then-uncommitted explicit
search-label candidate. The three modes completed with exit 0, `done: true`,
and zero problems. Each passed ten named journeys and 23 assertions, reached
the same 32 distinct bindings as the earlier run, and retained all mandatory
coverage and visible-payload checks. No harness coverage assertion was changed.

| Mode and report | Start time, UTC | Projected field paths | Result |
|---|---|---:|---|
| realistic | 20:40:50.854 | 216 | Exit 0; visible hostile strings, no injection signals |
| poison | 20:41:07.298 | 228 | Exit 0; invalid-event recovery and subsequent chooser reached |
| control | 20:41:09.740 | 216 | Exit 0; visible HTTPS text and actual control anchor |

All three runs used Windows, Node 24.13.0 and headless Chrome 152.0.0.0,
with the measured 1014×800 CSS-pixel viewport at device pixel ratio 1.
The runner checked source identity before and after each run; an independent
post-run comparison also matched every recorded file against the current tree.
All modes share this source-manifest SHA-256:

```text
9bef272859bb280d835f129bf428e48fe3a46e586af85500ed641d18e7e4c442
```

That content identity includes these production files:

```text
export.js:        c68c756e224f94337939e84774627318d7a76f42bca71d43b6a0aec8bf3304d2
picker-list.js:   229f144be5396c4d3028101acce138e4decff696fa7461389edc1f22cae0fdbd
picker-search.js: 6e5710f38d32d8611e007b401a514604ce20251265b6adfedca8e67581553bad
```

The generated schema hashes are unchanged from the earlier run. The earlier
run's reports and transcripts remain where they were written, outside this
repository, and these new outputs were written to a separate directory beside
them rather than over them. The linked JSON copies have the following SHA-256
hashes:

```text
realistic.json: 971bf7ad030fa6fa406e2f1786688096c290f7015ad442544670e65a6ff8ca65
poison.json:    08488e499c0b5376b27e6e373b22c77a3e4593b72fe60298d0a609e566a215d6
control.json:   ceb4b0a2e2c14fe0252711be0467de2132170e255e3409f9cfdd35c3f3fe80a1
```

These results extend the recorded Chrome rendering evidence to the specified
source bytes. They do not resolve the separate native WebKit/AT-SPI/Orca
investigation or establish native CSP, chooser, signing or filesystem behavior.
