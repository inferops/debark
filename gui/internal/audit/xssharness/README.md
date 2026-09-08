# The hostile-fixture harness

> The capture journals, screenshots and raw logs cited in this document are
> part of the project's internal records and are not published with the
> source. The measurements they back are reproduced here in full.

This harness boots the actual shell and screen modules in headless Chrome,
walks visible controls, and projects hostile presentation strings through a
fixture bridge. It tests rendering and reachability. Native choosers, Wails,
Go validation, filesystem work, signing and the production CSP are outside
this browser fixture's evidence.

The source audit in `internal/audit/domsinks_test.go` covers sinks that these
journeys do not reach. The current run is recorded in the desktop
simplification appendix of [the security review](../../../docs/security-review.md#desktop-simplification-hostile-ui-audit-2026-09-07).
Earlier second-round coverage and results in that document remain historical.

## Running it

Use Node with built-in `fetch` and Chrome; the recorded run used Node 24.13.0
and Chrome 152 on Windows. There are no npm dependencies.

```sh
cd internal/audit/xssharness
node run.mjs realistic
node run.mjs poison
node run.mjs control
```

The runner regenerates `bridge.json` from the current Wails-generated
`models.ts` and `App.d.ts` before each run. `node genfixture.mjs [output-path]`
is also available independently. The parser includes nested generic returns
such as `Promise<Array<any>>` and rejects incomplete method discovery.

`DEBARK_CHROME` overrides the executable. `DEBARK_XSS_PORT` overrides the
loopback port (default 7251). `DEBARK_XSS_REPORT` optionally names a JSON
report file; its parent directory must exist. For example:

```sh
DEBARK_XSS_REPORT=/tmp/debark-xss-realistic.json node run.mjs realistic
```

The generated bridge file lives beside the harness and can be deleted after
use. Chrome receives a fresh OS-temporary profile, which the runner removes
on exit after checking the exact path boundary. Windows child windows are
hidden. When run as root in the Linux audit container, Chrome receives
`--no-sandbox`; ordinary host runs retain its normal sandbox. This does not
change the native application's CSP or sandbox configuration.

A pass requires exit code 0, a complete structured report, and
`TOTAL PROBLEMS: 0`. Missing browser/server, incomplete execution, missing
required bindings, absent positive controls, JavaScript errors and injection
signals produce a nonzero exit. Source changes during a run invalidate it.

## Fixture projection and modes

`hostile.js` reuses the deterministic state machine in
`hack/ui-review/fixtures.mjs`. The fixture validates its results against the
generated schema before the hostile wrapper projects text. Actual booleans,
counts, revisions, stable identities and echoed search queries remain legal
so rendering paths can be reached. Empty or absent fields are not fabricated;
`projectedFields` records the presentation fields actually exercised.
Event-only envelopes absent from generated Wails models retain their legal
shape while nested presentation strings are projected.

- **realistic:** reachable presentation strings carry injection payloads;
  state, identity and query fields retain valid values.
- **poison:** performs the same journeys, then deliberately sends invalid enum
  and error event data. Authoritative fixture state recovers the view, and a
  subsequent visible Change action must reach the source chooser.
- **control:** replaces the same URL fields with a legitimate HTTPS URL. Both
  its visible text and a visible `doc_url` link must appear. This proves that
  rejecting a `javascript:` link did not merely skip the linking code.

Poison mode is a bounded error/recovery exercise. It does not claim to test
every possible malformed bridge response or every event ordering.

## Visible journeys and mandatory coverage

The ten journeys use visible, enabled, unambiguous controls, with scrolling
and modal scope checked before activation. Disclosures open through their
visible summaries; dialogs open through their actual actions. The driver does
not force `open`, call `shell.go` to bypass navigation, or click an arbitrary
collection of buttons.

| Journey | Required path exercised |
|---|---|
| Main menu → System check → Back | Current readiness report and its disclosures |
| Snapshot file → Choose → Continue | Chooser result, `InspectSnapshot`, `SelectTarget`, preparation |
| Search → Enter → Details → Close | `SearchPackages`, `GetPackage`, logical list focus restoration |
| Selected → Details | Paged selection and safe warning `doc_url` link handling |
| Add → Paste list | `ParsePackageList`, preview, `AddPackageList` |
| Add → URL | Reject `javascript:` input, then preserve a valid URL and SHA-256 in `AddURLs` |
| Add → local files | `ChooseLocalDebs` and exactly one two-file `AddLocalDebs` call |
| Review bundle → output/key choosers → Build | `ChooseDirectory`, `ChooseSigningKey`, collapsed/expanded validation, `PreviewCommand`, `StartBuild` and fixture completion |
| Copy to drive → explicit destination | `ListVolumes`, `InspectDestination`, `PlanExport` |
| Main menu → existing copy, then hostile errors/recovery | Source chooser, `ExportStatus`, `LifecycleStatus`, visible error details and a subsequent chooser call |

The table expands some actions within the report's ten named journeys. The
mandatory binding list in `harness.js` makes a missing path a failure. The
recorded runs reached 32 distinct methods from a schema of 53 methods and 55
classes. They do not cover all methods; for example native Quit, actual
verification, Undo and copy cancellation have separate tests.

## Detection and reporting

The scanner includes body-mounted tray dialogs and menus, not just `#app`.
Only the immutable harness bootstrap and report node are excluded.

1. A call to `window.__xss` records script execution.
2. Elements outside the application's allowed HTML/SVG tags record parsed
   markup.
3. Event-handler attributes, dangerous URL protocols and CSS `url()` values
   record attribute/style injection.
4. Off-origin resource entries and instrumented JS network requests record
   egress.

The positive check requires complete payload strings in visible text nodes:
description, label, path and stderr in every mode; the hostile URL in
realistic/poison; and HTTPS text plus its actual link in control. Hidden
content and text in the report cannot satisfy this condition.

Reports include each journey and reached call, distinct failure messages,
projected field paths, visible payloads, browser user agent, actual viewport,
Node/platform, source file hashes and generated-schema hashes. These are
coverage and rendering results, not native keyboard or screen-reader tests.

## Recorded desktop simplification run

All three modes completed with exit 0 and zero problems on 2026-09-07. Each
reached ten named journeys and 32 methods. Realistic/control projected 216
field paths; poison projected 228 and recovered to a reached chooser.
The actual viewport was 1014×800 CSS pixels at device pixel ratio 1; the
1040×900 launch argument is not substituted for that measurement.

Durable reports: realistic,
poison,
control.
All carry source manifest
`bddcd3538d9bfe45f522034e9800f151b875088b4a25e83f078cdef3a44000c6`.
These reports identify the exact snapshot, not every later commit.
