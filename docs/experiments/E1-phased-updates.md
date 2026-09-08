# E1 — Phased updates

**Status: complete.** Result **qualifies** ADR-006: the
mechanism works, but only for one of the two request paths the design relies
on it for.

## Question

Does setting `APT::Machine-ID` in a private apt root reproduce the phasing
decision the target's own apt would make? Is `APT::Machine-ID` honoured at
all by the apt versions debark will run?

Settles ADR-006.

## Method

Script: [`hack/experiments/e1-phased-updates.sh`](../../hack/experiments/e1-phased-updates.sh)
(rerunnable — the phasing set changes over time, so it discovers a live
candidate rather than hard-coding one).

1. Ran in `ubuntu:24.04` (digest `sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517`,
   apt **2.8.3**, dpkg 1.22.6), the container backend's own target release,
   2026-09-03.
2. Built a private apt root (explicit `Dir::*` options) with real
   `noble`/`noble-updates`/`noble-security`/`noble-backports` sources, forced
   to plain-text indexes so the script can grep them directly.
3. Searched the **real, live** `noble-updates/main` index for
   `Phased-Update-Percentage` rather than guessing a package name. Found 9
   candidates at the time of the recorded run (full list in
   `hack/experiments/out/e1/phased-candidates.txt`); picked the first
   alphabetically for a stable, reproducible choice: **`dnsmasq-base`
   2.91-0ubuntu0.24.04.1, phased at 40%**, with a non-phased fallback
   `2.90-2ubuntu0.4` in `noble-security`.
4. **Methodology pitfall found and corrected**: a synthetic one-line dpkg
   status file (just the target package, no real dependencies) makes
   `apt-get upgrade` refuse to touch the package for unrelated reasons
   (it won't pull in new packages to satisfy an upgrade), producing a false
   "always kept back" reading that has nothing to do with phasing. Fixed by
   really `apt-get install`-ing the fallback version into a throwaway
   container root first, then using *that* dependency-consistent
   `/var/lib/dpkg/status` as the private root's status.
5. Resolved with `apt-get -s` under a matrix of **command** (`upgrade`,
   `full-upgrade`, explicit `install <pkg>`) × **policy** (`APT::Machine-ID`
   = id A, = id B, `APT::Get::Never-Include-Phased-Updates=true`), then
   scanned 27 more deterministic machine-ids (sha256 of fixed seed strings,
   truncated to 32 hex chars, like a real `/etc/machine-id`) under
   `apt-get upgrade -s` to look for a direct split.

## Raw evidence

```text
apt-cache madison dnsmasq-base
dnsmasq-base | 2.91-0ubuntu0.24.04.1 | .../noble-updates/main amd64 Packages   (phased 40%)
dnsmasq-base | 2.90-2ubuntu0.4       | .../noble-security/main amd64 Packages
dnsmasq-base | 2.90-2build2          | .../noble/main amd64 Packages
```

Command × policy matrix (dpkg status: `dnsmasq-base 2.90-2ubuntu0.4` installed
with its real dependencies; machine-id A = `eea43c...`, B = `69046c...`):

| command | machine-id A | machine-id B | `Never-Include-Phased-Updates` |
|---|---|---|---|
| `apt-get upgrade -s` (automatic) | **selected 2.91** | **kept back 2.90** | kept back 2.90 |
| `apt-get full-upgrade -s` (automatic) | **selected 2.91** | **kept back 2.90** | kept back 2.90 |
| `apt-get install -s dnsmasq-base` (explicit name) | selected 2.91 | selected 2.91 | **selected 2.91** |

27-machine-id scan under `apt-get upgrade -s`:

```text
14/27 selected the phased version, 13/27 kept it back (index says 40% should select it)
```

(alpha/bravo/charlie/echo/foxtrot/hotel/juliet/mike/oscar/sierra/tango/victor/
whiskey/zulu → selected; delta/golf/india/kilo/lima/november/papa/quebec/
romeo/uniform/xray/yankee/omega → kept back — full per-id log in
`hack/experiments/out/e1/scan-*.log`)

Full run transcript: `hack/experiments/out/e1-run.log`. Confirmed the same
apt binary strings exist to support this (`strings` on
`libapt-pkg.so.6.0.0`, apt 2.8.3): `APT::Machine-ID`, `Dir::Etc::machine-id`,
`APT::Get::Never-Include-Phased-Updates`, `APT::Get::Always-Include-Phased-Updates`.
A file-based alternative, `-o Dir::Etc::machine-id=<path-to-file>`, was also
tested and produces identical results to `-o APT::Machine-ID=<value>` — either
mechanism works.

## Finding

**`APT::Machine-ID` is real, honoured, apt configuration — but it (and
`Never-Include-Phased-Updates`) only affects apt's *automatic* upgrade-
candidate selection (`apt-get upgrade` / `full-upgrade` with no package named
on the command line). An explicitly named request — `apt-get install <pkg>`,
with or without a version — always bypasses phasing entirely, regardless of
machine-id or `Never-Include-Phased-Updates`.** This is a real, repeatable
apt behaviour (confirmed identically across an empty dpkg status, a
synthetic single-package status, and a realistic dependency-consistent
status; not a testing artefact), not something specific to this experiment's
setup.

This has two distinct consequences for the two paths.1's own
resolution sequence:

1. **`full-upgrade --download-only` pass (the `--upgrades` request)** — this
   is exactly the automatic-selection path, and the mechanism works as
   ADR-006 describes: setting the target's machine-id changes the outcome
   (confirmed: A selects, B holds back, on the identical request), and
   `Never-Include-Phased-Updates=true` reliably holds the package back. The
   design's plan here is **validated**.
2. **`install --download-only <requested packages>` pass (the main,
   explicitly-named request path)** — phasing has **no effect at all**, in
   any of the three configurations tested. This cuts two ways:
   - It means there is **no divergence risk** for this path: a target admin
     who ran `apt-get install dnsmasq-base` themselves would *also* get the
     newest (phased) version regardless of their own machine-id, so the
     builder reproduces that exactly, machine-id or not.
   - But it also means the **conservative fallback promise in ADR-006 is not
     actually kept for this path**: ADR-006 says a redacted
     machine-id falls back to `Never-Include-Phased-Updates=true` so
     resolution "select[s] only fully-phased versions, which the target also
     accepts." That guarantee holds for the upgrade pass but **is false for
     the requested-packages pass** — `Never-Include-Phased-Updates=true` did
     not stop `apt-get install dnsmasq-base` from resolving to the
     not-fully-rolled-out 2.91 in every trial.

## What this means for the design

- The phased-update policy (ADR-006) should be **narrowed, not discarded**:
  state explicitly that the machine-id / never-include policy governs the `full-upgrade`
  (`--upgrades`) pass only. It does not apply to, and cannot be used to
  protect, the explicitly-requested-package pass — but that pass also does
  not need it, since apt's own bypass-on-explicit-name behaviour means the
  builder and a hypothetical target-side `apt-get install <pkg>` always
  agree regardless of machine-id.
- If debark's actual intent is "never let a redacted-machine-id build ship
  a not-fully-phased version of an *explicitly requested* package either,"
  the current design (the fallback) does **not** achieve that. Achieving
  it would require an extra post-resolution check in Go: read each
  `Phased-Update-Percentage` from the `Packages` index for the resolved
  `Selection`s and warn/reject/pin-to-fallback any that are `< 100` when the
  snapshot's machine-id was redacted. This is a real gap the project
  should either accept explicitly (document the limitation) or close with
  this extra check — as written, it silently doesn't do what its own prose
  claims for the named-package path.
- Record which policy applied in the lock, as ADR-006 already says — but the
  lock/evidence should also record **which pass** (upgrade vs.
  requested-package) each phased selection came through, since the two have
  materially different guarantees.
