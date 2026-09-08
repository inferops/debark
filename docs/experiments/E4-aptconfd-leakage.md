# E4 — `apt.conf.d` leakage

**Status: complete.** Result **confirms and sharpens** the premise behind
ADR-005 (snapshot captures `apt.conf.d`) — and finds a second, more
fundamental problem: the private-root idiom the design describes
(`-o Dir::*` overrides) **cannot carry `apt.conf.d` at all**, by construction
of how apt loads its configuration. This is independent of whether debark
remembers to capture the directory; it is about how to *feed it back to apt*
once captured.

## Question

Does the prototype (which does not capture `apt.conf.d`) resolve a
different, larger set than the target would for a target with
`APT::Install-Recommends "false";` and a `Default-Release`? Does carrying
`apt.conf.d` fix it? Quantified: how many extra packages, how many extra
megabytes, for a realistic request.

Settles the snapshot field set (ADR-005).

## Method

Script: [`hack/experiments/e4-aptconfd-leakage.sh`](../../hack/experiments/e4-aptconfd-leakage.sh).
Run in `ubuntu:24.04` (digest `sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517`,
apt **2.8.3**), 2026-09-03.

**Confirmed by reading it**: `download-packages.sh` (the validated prototype)
copies `dpkg-status`, `apt/sources.list*`, `apt/preferences*` and
`apt/trusted.gpg.d/*` from a `--state` snapshot — it never copies
`apt.conf.d`. Its own `--no-recommends` flag is a manual operator override,
not something derived from the target's captured configuration.

**Part 1 — Install-Recommends, a realistic request.** Built a target apt.conf.d
carrying `APT::Install-Recommends "false";`. Resolved `apt-get install
--print-uris -qq -y git` (git has a real Recommends chain: `ca-certificates,
patch, less, ssh-client`, a realistic mid-size request) against a private
apt root (explicit `Dir::*` options, empty dpkg status for a clean
fresh-install count) under four configurations:

- **(a) target ground truth** — recommends=false actually in effect.
- **(b) prototype today** — no apt.conf.d carried at all (default
  Install-Recommends=true).
- **(b2) the "obvious" fix** — `-o Dir::Etc::parts=<captured apt.conf.d dir>`
  added to the same `-o Dir::*` idiom the design uses for every other path
  (`sourcelist`, `sourceparts`, `preferences`, `trustedparts`, `State`,
  `Cache`, `Log` — all of which *do* respond to `-o` overrides, confirmed
  working throughout E1/E3/E6).
- **(c) the fix that actually works** — see below.

Package count and byte totals taken from `--print-uris` output (the same
output the design says debark parses into the lock), summing the `Size` field
apt reports for every file it would fetch.

**The apt.conf.d mechanism, investigated directly.** Before trusting (b2),
checked whether `-o Dir::Etc::parts=<dir>` (or `-o Dir::Etc=<parent>`, or the
`-c <file>` / `--config-file=<file>` command-line flag) actually redirects
where apt scans for `apt.conf.d`, using `apt-config dump` to inspect the
resulting `APT::Install-Recommends` value directly, isolated from the full
resolution:

```text
apt-config -o Dir::Etc::parts=/tmp/confd dump | grep -iE "parts|Recommends"
  Dir::Etc::parts "/tmp/confd";        <- the key itself DOES show the override
  APT::Install-Recommends "1";         <- but the file inside it was NOT read

APT_CONFIG=/tmp/loader.conf apt-config dump | grep -iE "parts|Recommends"
  (loader.conf contains: Dir::Etc::parts "/tmp/confd";)
  Dir::Etc::parts "/tmp/confd";
  APT::Install-Recommends "false";     <- this time it WAS read

apt-get -c /tmp/loader.conf install --print-uris -qq -y git | grep -c "^'http"
  48   <- unchanged from baseline (48) — -c does NOT work either

APT_CONFIG=/tmp/loader.conf apt-get install --print-uris -qq -y git | grep -c "^'http"
  24   <- APT_CONFIG env var DOES work, and matches -o APT::Install-Recommends=false directly (24)
```

Conclusion of this sub-test: apt reads its main `apt.conf` and `apt.conf.d/*`
at a bootstrap stage that runs **before** command-line `-o` and `-c`/
`--config-file` processing (those flags exist to override individual
*values* after the fact, not to redirect *which files get read* for the
conf.d scan itself — by the time `-o`/`-c` are applied, the conf.d directory
has already been enumerated using whatever `Dir::Etc::parts` was in scope at
process start, i.e. the compiled-in default `/etc/apt/apt.conf.d`). The one
mechanism that works is the **`APT_CONFIG` environment variable**, pointed
at a small generated "loader" file whose content sets `Dir::Etc::parts` (and
`Dir::Etc::main`, if a captured main `apt.conf` exists) to the snapshot's
captured paths — because `APT_CONFIG` is consulted at that same early
bootstrap stage, just early enough to still change what gets read next. This
composes cleanly with the rest of the private-root `-o Dir::*` flags (tested
together, no conflict).

**Part 2 — `Default-Release`**, on a small fully-controlled two-suite
fixture (two local flat repos, `suite-low` and `suite-high`, each publishing
a *different* version of the same placeholder package built with
`dpkg-deb --build`, `suite-high`'s version deliberately numbered *lower* so
version-number-only selection and Default-Release-driven selection disagree)
— so the result does not depend on today's Ubuntu archive contents.

## Raw evidence

Part 1 (full transcript: `hack/experiments/out/e4-run.log`):

```text
target (recommends=false)          ->   50 files,   28098552 bytes (26.8 MB)
prototype (no apt.conf.d carried)  ->  110 files,   39167590 bytes (37.4 MB)
'-o Dir::Etc::parts=...' (broken)  ->  110 files,   39167590 bytes (37.4 MB)
APT_CONFIG=loader (fixed)          ->   50 files,   28098552 bytes (26.8 MB)

delta (prototype minus target):     60 extra file(s), 11069038 extra bytes (10.56 MB)
```

For a single `git` install request on a stock Ubuntu 24.04 fixture: the
prototype's failure to carry `apt.conf.d` pulls in **60 extra packages and
10.6 MB it should not have (a 122% file-count / 39% byte-size overshoot)**,
purely from one missing `Install-Recommends "false";` line. The broken `-o`
"fix" changes nothing (bit-for-bit identical to the unfixed prototype: 110
files / 39167590 bytes both times) — confirming it is not a fix at all, just
a no-op that would look like one to anyone who didn't check the byte counts.
The `APT_CONFIG` loader-file fix reproduces the target's real 50-file/26.8 MB
result exactly.

Part 2 (full transcript same file, "part 2" section):

```text
policy without Default-Release:
  Candidate: 1.0-low          <- picked the higher VERSION NUMBER, from suite-low

policy with APT::Default-Release="suite-high" (carried via the working
APT_CONFIG mechanism):
  Candidate: 0.9-high         <- picked the lower-numbered version, because
                                  Default-Release named its suite
```

## Finding

**Confirmed on both counts asked for**: the prototype resolves a materially
larger set (60 extra files, 10.6 MB, +39% bytes for one mid-size request)
when a target's `Install-Recommends "false";` isn't carried, and
`Default-Release` independently and silently changes *which version* gets
selected (`1.0-low` → `0.9-high` in the controlled fixture) — invisible to
any resolver that doesn't carry `apt.conf.d`.

**A more fundamental and non-obvious second finding**: even a debark
implementation that *does* capture `apt.conf.d` into the snapshot cannot
feed it back to apt using the same `-o Dir::Etc::*=<path>` idiom that works
for every other private-root path (sources, preferences, trusted keys, state,
cache). That idiom is silently a no-op for `apt.conf`/`apt.conf.d`
specifically, because of when apt reads them relative to command-line
parsing. The only mechanism found to work is the `APT_CONFIG` environment
variable pointed at a small generated loader file.

## What this means for the design

- The snapshot-capture rule and ADR-005 are **directionally correct but
  incomplete**: capturing `apt.conf.d` is necessary (quantitatively
  confirmed) but **not sufficient**
  — the design's implied mechanism for applying it (the blanket "invoke
  apt-get with explicit `Dir::*` options") does not work for this specific
  path and needs its own documented mechanism.
- **Concrete implementation requirement for the `core/apt` local backend**:
  when materialising the private root, generate a small `APT_CONFIG` loader
  file (e.g. `ROOT/debark-apt.conf`) containing
  `Dir::Etc::parts "<ROOT>/etc/apt/apt.conf.d";` (and `Dir::Etc::main` if a
  captured main `apt.conf` file exists), and set the `APT_CONFIG`
  **environment variable** — not a command-line flag — for every `apt-get`/
  `apt-cache` invocation against that root. This applies to
  `core/apt/iface.go`'s `Runner.Run` implementation and to the closed-world
  check equally, and to the container backend's inner
  `debark build --backend=local` invocation.
- Recommend adding this as its own numbered point, since it affects every
  resolution the local/container backend ever runs, not just the
  Recommends/Default-Release cases tested here — any
  target apt.conf.d setting (proxies already redacted per ADR-005, pinning
  helpers, `Acquire::*` retry/timeout tuning, etc.) is silently dropped
  without it.
- Consider a unit test (once `core/apt` implements the local backend) that
  asserts the private root is invoked with `APT_CONFIG` set and not solely `-o
  Dir::Etc::parts=`, to prevent silent regression back to the no-op form —
  the two are byte-identical in every other way and this is exactly the kind
  of bug that would pass a superficial code review.
